package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/GhandyP/project-pipeline-agent/internal/agent"
	"github.com/GhandyP/project-pipeline-agent/internal/domain"
	"github.com/GhandyP/project-pipeline-agent/internal/ports"
)

// runReview reviews one project in the terminal. It cannot write: the terminal
// path has no decision command on purpose, and the agent has no write tool.
func runReview(args []string, out io.Writer) error {
	flags := flagSet("review", out)
	project := flags.String("project", "", "project id to review (required)")
	dataDir := flags.String("data", "", "directory holding the tracker")
	scripted := flags.Bool("scripted", false, "use the deterministic scripted model instead of Gemini")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*project) == "" {
		return errors.New("--project is required")
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if *dataDir != "" {
		cfg.dataDir = *dataDir
	}

	ctx := context.Background()
	llm, err := cfg.llm(ctx, *scripted)
	if err != nil {
		return err
	}
	_, svc, err := openStore(cfg, "", out)
	if err != nil {
		return err
	}
	reviewer, err := agent.NewReviewer(llm, svc)
	if err != nil {
		return err
	}

	snapshot, err := svc.Snapshot(ctx, *project)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s (%s) at version %d, %d milestone(s)\n", snapshot.Name, snapshot.ProjectID, snapshot.Version, len(snapshot.Milestones))
	fmt.Fprintln(out, "Reviewing. This command writes nothing.")

	var review *domain.PipelineReview
	if err := reviewer.Stream(ctx, *project, streamTo(out, &review)); err != nil {
		return err
	}
	if review == nil {
		return errors.New("the review ended without a review")
	}
	printReview(out, review)
	fmt.Fprintln(out, "\nNothing was written. Approve an update from the web app (`pipeline serve`).")
	return nil
}

// streamTo prints one review event and keeps the grounded review for the
// summary that follows.
func streamTo(out io.Writer, review **domain.PipelineReview) func(ports.ReviewEvent) error {
	return func(event ports.ReviewEvent) error {
		switch event.Kind {
		case ports.EventStart:
			fmt.Fprintf(out, "\n• %s\n", event.Text)
		case ports.EventText:
			fmt.Fprintf(out, "  model: %s\n", event.Text)
		case ports.EventToolCall:
			fmt.Fprintf(out, "  → %s %s\n", event.Tool, event.Args)
		case ports.EventToolResult:
			fmt.Fprintf(out, "  ← %s\n", event.Tool)
		case ports.EventError:
			fmt.Fprintf(out, "  ! %s\n", event.Error)
		case ports.EventReview:
			*review = event.Review
		}
		return nil
	}
}

// printReview renders the review the way a reviewer reads it: the rules' view
// first, then what the model made of it, then what it refused to accept.
func printReview(out io.Writer, review *domain.PipelineReview) {
	findings := review.Findings
	fmt.Fprintf(out, "\nFindings derived at version %d — health %s\n", findings.Version, findings.Health)
	if len(findings.Blockers) == 0 {
		fmt.Fprintln(out, "  no blockers")
	}
	for _, blocker := range findings.Blockers {
		fmt.Fprintf(out, "  [%s] %s %s\n", blocker.Severity, blocker.Kind, blocker.MilestoneID)
		fmt.Fprintf(out, "        %s\n", blocker.Summary)
		fmt.Fprintf(out, "        owner state: %s\n", blocker.OwnerStatus)
		for _, evidence := range blocker.Evidence {
			fmt.Fprintf(out, "        evidence: %s — %s\n", evidence.Rule, evidence.Detail)
		}
	}
	for _, item := range findings.Overdue {
		fmt.Fprintf(out, "  overdue: %s by %d day(s) [%s]\n", item.MilestoneID, item.DaysOverdue, item.Severity)
	}

	fmt.Fprintf(out, "\nSummary: %s\n", review.Summary)
	if len(review.NextActions) > 0 {
		fmt.Fprintln(out, "Ranked next actions")
		for _, action := range review.NextActions {
			owner := ""
			if !action.Owner.IsZero() {
				owner = " (owner " + action.Owner.Name + ")"
			}
			fmt.Fprintf(out, "  %d. %s%s — cites %s\n", action.Rank, action.Action, owner, strings.Join(action.BlockerIDs, ", "))
		}
	}
	for _, proposal := range review.Proposals {
		fmt.Fprintf(out, "\nProposed dry run %s against version %d\n", proposal.UpdateID, proposal.ExpectedVersion)
		if proposal.Rationale != "" {
			fmt.Fprintf(out, "  rationale: %s\n", proposal.Rationale)
		}
		for _, change := range proposal.Changes {
			fmt.Fprintf(out, "  %s: %s %q → %q\n", change.MilestoneID, change.Field, change.Before, change.After)
		}
	}
	if len(review.Proposals) == 0 {
		fmt.Fprintln(out, "\nNo update was proposed.")
	}
	if len(review.Rejections) > 0 {
		fmt.Fprintln(out, "\nRefused by the deterministic gate")
		for _, rejection := range review.Rejections {
			fmt.Fprintf(out, "  %s %s: %s\n", rejection.Kind, rejection.Subject, rejection.Reason)
		}
	}
}
