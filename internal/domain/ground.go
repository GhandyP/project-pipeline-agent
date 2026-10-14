package domain

import (
	"fmt"
	"strings"
	"time"
)

// MaxNextActions caps the ranked action list. A review that ranks more than
// seven actions is not a decision aid.
const MaxNextActions = 7

// ModelChange is a field-level intention the model proposes. It carries no
// before value: the deterministic side fills that in from the tracker, so a
// model cannot invent what the tracker currently says.
type ModelChange struct {
	MilestoneID string `json:"milestone_id"`
	Field       string `json:"field"`
	Value       string `json:"value"`
}

// ModelAction is a ranked intention the model proposes. It must cite the
// blockers it addresses by id.
type ModelAction struct {
	Action     string   `json:"action"`
	BlockerIDs []string `json:"blocker_ids"`
	OwnerID    string   `json:"owner_id,omitempty"`
	Rationale  string   `json:"rationale,omitempty"`
}

// ModelReview is the whole of what a model may author: a summary, ranked
// actions that cite derived blockers, and dry-run change intentions.
type ModelReview struct {
	Summary     string        `json:"summary"`
	NextActions []ModelAction `json:"next_actions,omitempty"`
	Changes     []ModelChange `json:"changes,omitempty"`
}

// Ground is the deterministic gate between model output and anything a human
// reads or approves.
//
// It admits a next action only when it names at least one blocker the rules
// actually derived, and it passes changes through ProposeChanges. Everything
// refused lands in Rejections with the reason, so a refusal is visible instead
// of silent. Ground never fails: a bad model answer produces a review with the
// rules' findings and the refusals, which is exactly what a reviewer needs.
func Ground(snapshot ProjectSnapshot, findings Findings, model ModelReview, now time.Time) PipelineReview {
	now = now.UTC()
	review := PipelineReview{Findings: findings}

	known := map[string]Blocker{}
	for _, blocker := range findings.Blockers {
		known[blocker.ID] = blocker
	}

	type candidate struct {
		action  NextAction
		order   int
		best    int
		seenKey string
	}
	var candidates []candidate
	seen := map[string]bool{}

	for index, raw := range model.NextActions {
		text := strings.TrimSpace(raw.Action)
		if text == "" {
			review.Rejections = append(review.Rejections, Rejection{Kind: "next_action", Subject: fmt.Sprintf("action %d", index+1), Reason: "the action text is empty"})
			continue
		}
		if len(raw.BlockerIDs) == 0 {
			review.Rejections = append(review.Rejections, Rejection{Kind: "next_action", Subject: text, Reason: "the action cites no blocker id, so it cannot be traced to a finding"})
			continue
		}

		cited := make([]Blocker, 0, len(raw.BlockerIDs))
		ids := make([]string, 0, len(raw.BlockerIDs))
		unknown := make([]string, 0)
		for _, id := range raw.BlockerIDs {
			blocker, ok := known[strings.TrimSpace(id)]
			if !ok {
				unknown = append(unknown, id)
				continue
			}
			if !containsID(ids, blocker.ID) {
				ids = append(ids, blocker.ID)
				cited = append(cited, blocker)
			}
		}
		if len(unknown) > 0 {
			review.Rejections = append(review.Rejections, Rejection{Kind: "next_action", Subject: text, Reason: fmt.Sprintf("these ids are not blockers of this review: %s", strings.Join(unknown, ", "))})
			continue
		}

		owner := Owner{}
		if id := strings.TrimSpace(raw.OwnerID); id != "" {
			member, ok := snapshot.TeamMember(id)
			if !ok {
				review.Rejections = append(review.Rejections, Rejection{Kind: "next_action", Subject: text, Reason: fmt.Sprintf("owner_id %q is not on the project team", id)})
				continue
			}
			owner = member
		}

		key := text + "|" + strings.Join(ids, ",")
		if seen[key] {
			review.Rejections = append(review.Rejections, Rejection{Kind: "next_action", Subject: text, Reason: "the same action is proposed twice"})
			continue
		}
		seen[key] = true

		best := 0
		var evidence []Evidence
		for _, blocker := range cited {
			if blocker.Severity.Rank() > best {
				best = blocker.Severity.Rank()
			}
			for _, item := range blocker.Evidence {
				if !containsEvidence(evidence, item) {
					evidence = append(evidence, item)
				}
			}
		}
		candidates = append(candidates, candidate{
			order: index,
			best:  best,
			action: NextAction{
				Action:     text,
				Owner:      owner,
				BlockerIDs: ids,
				Rationale:  strings.TrimSpace(raw.Rationale),
				Evidence:   evidence,
			},
		})
	}

	// Rank by the worst blocker each action addresses, then by the order the
	// model proposed them, so the ranking is deterministic.
	stableSortBy(candidates, func(a, b candidate) bool {
		if a.best != b.best {
			return a.best > b.best
		}
		return a.order < b.order
	})
	if len(candidates) > MaxNextActions {
		for _, extra := range candidates[MaxNextActions:] {
			review.Rejections = append(review.Rejections, Rejection{Kind: "next_action", Subject: extra.action.Action, Reason: fmt.Sprintf("more than %d actions were proposed", MaxNextActions)})
		}
		candidates = candidates[:MaxNextActions]
	}
	for i, item := range candidates {
		action := item.action
		action.Rank = i + 1
		review.NextActions = append(review.NextActions, action)
	}

	proposal, refusals, err := ProposeChanges(snapshot, now, model.Summary, model.Changes)
	review.Rejections = append(review.Rejections, refusals...)
	if err == nil {
		review.Proposals = append(review.Proposals, proposal)
	} else if err != ErrNoChanges {
		review.Rejections = append(review.Rejections, Rejection{Kind: "change", Subject: "update", Reason: err.Error()})
	}

	review.Summary = strings.TrimSpace(model.Summary)
	if review.Summary == "" {
		review.Summary = defaultSummary(review.Findings)
	}
	return review
}

// defaultSummary is used when the model wrote nothing, so the review a human
// reads is never empty even when the model failed.
func defaultSummary(findings Findings) string {
	if len(findings.Blockers) == 0 {
		return fmt.Sprintf("No blockers were derived for %s at version %d. Nothing needs attention.", findings.ProjectID, findings.Version)
	}
	counts := map[Severity]int{}
	for _, blocker := range findings.Blockers {
		counts[blocker.Severity]++
	}
	parts := make([]string, 0, len(AllSeverities()))
	for _, severity := range AllSeverities() {
		if counts[severity] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[severity], severity))
		}
	}
	return fmt.Sprintf("%d blocker(s) derived for %s at version %d: %s; %d overdue item(s).",
		len(findings.Blockers), findings.ProjectID, findings.Version, strings.Join(parts, ", "), len(findings.Overdue))
}

func containsID(ids []string, id string) bool {
	for _, known := range ids {
		if known == id {
			return true
		}
	}
	return false
}

func containsEvidence(list []Evidence, item Evidence) bool {
	for _, known := range list {
		if known == item {
			return true
		}
	}
	return false
}

func stableSortBy[T any](items []T, less func(a, b T) bool) {
	// Insertion sort: stable, allocation-free and plenty for at most a handful
	// of actions, which keeps the ordering rules easy to read.
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && less(items[j], items[j-1]); j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}
