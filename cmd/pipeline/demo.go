package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/GhandyP/project-pipeline-agent/internal/agent"
	"github.com/GhandyP/project-pipeline-agent/internal/domain"
	"github.com/GhandyP/project-pipeline-agent/internal/service"
)

// runDemo walks the whole workflow offline: the real rules, the real ADK runner
// and the real tracker, driven by the deterministic scripted model over a
// temporary data directory.
//
// It is the one path that both reviews and decides, and it decides only after a
// named human answers the prompt. Without a terminal it declines, because a
// pipe is not a person.
func runDemo(args []string, stdin io.Reader, out io.Writer) error {
	flags := flagSet("demo", out)
	project := flags.String("project", "delta-onboarding", "project id to walk through")
	fixtures := flags.String("fixtures", "", "fixture directory to seed from (default "+defaultFixtures+")")
	dataDir := flags.String("data", "", "data directory to use (default: a temporary one, removed afterwards)")
	as := flags.String("as", "", "name recorded on the approval (default: the first member of the project team)")
	approve := flags.Bool("approve", false, "approve the proposed update without prompting")
	if err := flags.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	fixtureDir := cfg.fixtures
	if *fixtures != "" {
		fixtureDir = *fixtures
	}

	cleanup := func() {}
	if *dataDir != "" {
		cfg.dataDir = *dataDir
	} else {
		temp, err := os.MkdirTemp("", "pipeline-demo-")
		if err != nil {
			return fmt.Errorf("create a temporary data directory: %w", err)
		}
		cfg.dataDir = temp
		cleanup = func() { os.RemoveAll(temp) }
	}
	defer cleanup()

	_, svc, err := openStore(cfg, fixtureDir, out)
	if err != nil {
		return err
	}
	reviewer, err := agent.NewReviewer(agent.NewScriptedModel(), svc)
	if err != nil {
		return err
	}

	ctx := context.Background()
	snapshot, err := svc.Snapshot(ctx, *project)
	if err != nil {
		return err
	}
	if strings.TrimSpace(*as) == "" {
		member, ok := firstTeamMember(snapshot)
		if !ok {
			return fmt.Errorf("project %q has no team, so nobody can approve an update for it; add a team member first", *project)
		}
		*as = member
	}

	fmt.Fprintln(out, "Offline walkthrough: the scripted model, the real rules, the real tracker.")
	fmt.Fprintln(out, "Nothing here can reach a network or a provider.")
	fmt.Fprintf(out, "Project %s (%s) at version %d; approver %s, from the project team.\n",
		snapshot.Name, snapshot.ProjectID, snapshot.Version, *as)

	var review *domain.PipelineReview
	if err := reviewer.Stream(ctx, *project, streamTo(out, &review)); err != nil {
		return err
	}
	if review == nil {
		return errors.New("the review ended without a review")
	}
	printReview(out, review)

	if len(review.Proposals) == 0 {
		fmt.Fprintln(out, "\nNothing to decide: the review proposed no update.")
		return printAudit(ctx, svc, *project, out)
	}
	proposal := review.Proposals[0]

	approved, err := askApproval(stdin, out, proposal, *as, *approve)
	if err != nil {
		return err
	}

	applied, record, err := svc.Decide(ctx, *project, proposal.UpdateID, service.Decision{
		Approved: approved,
		By:       *as,
		Note:     "offline demo",
	})
	if err != nil {
		return err
	}
	if approved {
		fmt.Fprintf(out, "\nApplied %s by %s: version %d → %d\n", applied.UpdateID, applied.Approval.By, applied.ExpectedVersion, applied.ResultingVersion)
	} else {
		fmt.Fprintf(out, "\nDeclined %s by %s. The project was not touched.\n", record.UpdateID, record.Approval.By)
	}

	final, err := svc.Snapshot(ctx, *project)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Project %s is at version %d.\n", final.ProjectID, final.Version)
	return printAudit(ctx, svc, *project, out)
}

// firstTeamMember is the demo's default approver. Approving is a project-owner
// decision, so the demo names a member of the team instead of borrowing the
// shell's user, which would be refused.
func firstTeamMember(snapshot domain.ProjectSnapshot) (string, bool) {
	for _, member := range snapshot.Team {
		if name := strings.TrimSpace(member.Name); name != "" {
			return name, true
		}
		if id := strings.TrimSpace(member.ID); id != "" {
			return id, true
		}
	}
	return "", false
}

// askApproval decides who answers the approval question: the flag, a terminal,
// or nobody.
func askApproval(stdin io.Reader, out io.Writer, proposal domain.ProposedUpdate, as string, preapproved bool) (bool, error) {
	if preapproved {
		if strings.TrimSpace(as) == "" {
			return false, errors.New("--approve needs a name: pass --as <name> or unset USER")
		}
		fmt.Fprintf(out, "\n--approve: approving %s as %s.\n", proposal.UpdateID, as)
		return true, nil
	}

	fmt.Fprintf(out, "\nApply %s as %s? [y/N] ", proposal.UpdateID, as)
	reader := bufio.NewReader(stdin)
	answer, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true, nil
	case "":
		fmt.Fprintln(out, "No answer was read, which is not an approval: declining and writing nothing.")
		return false, nil
	default:
		return false, nil
	}
}

func printAudit(ctx context.Context, svc *service.Service, projectID string, out io.Writer) error {
	records, err := svc.Audit(ctx, projectID)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "\nAudit trail for %s (%d record(s))\n", projectID, len(records))
	for _, record := range records {
		version := fmt.Sprintf("v%d", record.ExpectedVersion)
		if record.ResultingVersion > 0 {
			version = fmt.Sprintf("v%d → v%d", record.ExpectedVersion, record.ResultingVersion)
		}
		by := record.Approval.By
		if strings.TrimSpace(by) == "" {
			by = "—"
		}
		fmt.Fprintf(out, "  %-9s %s  %s  by %s  %s\n", record.Action, record.UpdateID, version, by, record.At.Format("2006-01-02 15:04:05Z"))
		if record.Digest != "" {
			fmt.Fprintf(out, "            %s\n", record.Digest)
		}
	}
	return nil
}
