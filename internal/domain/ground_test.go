package domain

import (
	"strings"
	"testing"
)

// grounded is a helper: derive the findings of a fixture and run them through
// the gate with the model answer under test.
func grounded(t *testing.T, fixture string, model ModelReview) PipelineReview {
	t.Helper()
	snapshot := loadFixture(t, fixture)
	findings, err := Derive(snapshot, referenceNow, DefaultPolicy())
	if err != nil {
		t.Fatalf("Derive(%s): %v", fixture, err)
	}
	return Ground(snapshot, findings, model, referenceNow)
}

func actionTexts(actions []NextAction) []string {
	out := []string{}
	for _, action := range actions {
		out = append(out, action.Action)
	}
	return out
}

func rejectionReasons(review PipelineReview) string {
	var parts []string
	for _, rejection := range review.Rejections {
		parts = append(parts, rejection.Kind+":"+rejection.Subject+":"+rejection.Reason)
	}
	return strings.Join(parts, " | ")
}

// TestGroundKeepsTheRulesAsTheOnlySourceOfFindings is the core safety property:
// whatever the model writes, the findings a human reads are the ones Go derived.
func TestGroundKeepsTheRulesAsTheOnlySourceOfFindings(t *testing.T) {
	review := grounded(t, "blocked", ModelReview{
		Summary: "The console is late and the reset boundary is blocked.",
		NextActions: []ModelAction{
			{Action: "Move the reset boundary to the next release", BlockerIDs: []string{"overdue:m-1"}, OwnerID: "u-luis"},
		},
	})

	if got, want := describe(review.Findings.Blockers), []string{
		"blocked_dependency/m-2/high/assigned",
		"overdue/m-1/high/assigned",
	}; !equalStrings(got, want) {
		t.Fatalf("findings = %v, want %v", got, want)
	}
	if review.Findings.Health != HealthAtRisk {
		t.Errorf("health = %q, want %q", review.Findings.Health, HealthAtRisk)
	}
	if review.Summary != "The console is late and the reset boundary is blocked." {
		t.Errorf("summary = %q, want the model's summary", review.Summary)
	}
	if len(review.Rejections) != 0 {
		t.Errorf("rejections = %s, want none", rejectionReasons(review))
	}
	if len(review.NextActions) != 1 {
		t.Fatalf("next actions = %+v, want one", review.NextActions)
	}
	action := review.NextActions[0]
	if action.Rank != 1 || action.Owner.ID != "u-luis" {
		t.Errorf("action = %+v, want rank 1 owned by u-luis", action)
	}
	if len(action.Evidence) == 0 {
		t.Fatal("the action carries no evidence from the blockers it cites")
	}
	for _, evidence := range action.Evidence {
		if evidence.ProjectID != "cobalt-billing" {
			t.Errorf("evidence = %+v, want it to cite the project", evidence)
		}
	}
}

// TestGroundRefusesUntraceableActions covers every way an action can fail to be
// traceable, one case per rule.
func TestGroundRefusesUntraceableActions(t *testing.T) {
	cases := []struct {
		name    string
		action  ModelAction
		wantErr string
	}{
		{
			name:    "empty text",
			action:  ModelAction{Action: "   ", BlockerIDs: []string{"overdue:m-1"}},
			wantErr: "the action text is empty",
		},
		{
			name:    "no blocker cited",
			action:  ModelAction{Action: "Escalate to the steering committee"},
			wantErr: "cites no blocker id",
		},
		{
			name:    "an id that is not a blocker",
			action:  ModelAction{Action: "Replan everything", BlockerIDs: []string{"overdue:m-9"}},
			wantErr: "these ids are not blockers of this review: overdue:m-9",
		},
		{
			name:    "a real id plus an invented one",
			action:  ModelAction{Action: "Fix both", BlockerIDs: []string{"overdue:m-1", "stalled:m-7"}},
			wantErr: "stalled:m-7",
		},
		{
			name:    "an owner who is not on the team",
			action:  ModelAction{Action: "Reassign", BlockerIDs: []string{"overdue:m-1"}, OwnerID: "u-ghost"},
			wantErr: `owner_id "u-ghost" is not on the project team`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			review := grounded(t, "blocked", ModelReview{NextActions: []ModelAction{tc.action}})
			if len(review.NextActions) != 0 {
				t.Fatalf("next actions = %+v, want none", review.NextActions)
			}
			reasons := rejectionReasons(review)
			if !strings.Contains(reasons, tc.wantErr) {
				t.Errorf("rejections = %s, want them to contain %q", reasons, tc.wantErr)
			}
			// The refusals never remove the rules' findings.
			if len(review.Findings.Blockers) != 2 {
				t.Errorf("findings = %d blockers, want the derived 2", len(review.Findings.Blockers))
			}
		})
	}
}

func TestGroundRefusesADuplicatedAction(t *testing.T) {
	action := ModelAction{Action: "Re-plan m-2", BlockerIDs: []string{"overdue:m-1"}}
	review := grounded(t, "blocked", ModelReview{NextActions: []ModelAction{action, action}})

	if len(review.NextActions) != 1 {
		t.Fatalf("next actions = %+v, want one", review.NextActions)
	}
	if !strings.Contains(rejectionReasons(review), "the same action is proposed twice") {
		t.Errorf("rejections = %s, want the duplicate refusal", rejectionReasons(review))
	}
}

// TestGroundRanksByTheWorstBlockerAnActionAddresses pins the ranking rule.
func TestGroundRanksByTheWorstBlockerAnActionAddresses(t *testing.T) {
	review := grounded(t, "contradictory", ModelReview{
		NextActions: []ModelAction{
			{Action: "Record why the dashboards are blocked", BlockerIDs: []string{"contradictory_status:m-2"}},
			{Action: "Correct the extract progress", BlockerIDs: []string{"contradictory_status:m-1"}},
		},
	})

	if got, want := actionTexts(review.NextActions), []string{
		"Correct the extract progress",
		"Record why the dashboards are blocked",
	}; !equalStrings(got, want) {
		t.Fatalf("action order = %v, want %v", got, want)
	}
	if review.NextActions[0].Rank != 1 || review.NextActions[1].Rank != 2 {
		t.Errorf("ranks = %d, %d; want 1 and 2", review.NextActions[0].Rank, review.NextActions[1].Rank)
	}
}

func TestGroundRanksTiesByTheModelOrder(t *testing.T) {
	review := grounded(t, "blocked", ModelReview{
		NextActions: []ModelAction{
			{Action: "First proposed", BlockerIDs: []string{"overdue:m-1"}},
			{Action: "Second proposed", BlockerIDs: []string{"blocked_dependency:m-2"}},
		},
	})
	if got, want := actionTexts(review.NextActions), []string{"First proposed", "Second proposed"}; !equalStrings(got, want) {
		t.Fatalf("action order = %v, want the model order %v", got, want)
	}
}

func TestGroundCapsTheActionList(t *testing.T) {
	var actions []ModelAction
	for i := 0; i < MaxNextActions+2; i++ {
		actions = append(actions, ModelAction{
			Action:     strings.Repeat("action ", 1) + itoa(i),
			BlockerIDs: []string{"overdue:m-1"},
		})
	}
	review := grounded(t, "blocked", ModelReview{NextActions: actions})

	if len(review.NextActions) != MaxNextActions {
		t.Fatalf("next actions = %d, want the cap of %d", len(review.NextActions), MaxNextActions)
	}
	for i, action := range review.NextActions {
		if action.Rank != i+1 {
			t.Errorf("action %d has rank %d, want %d", i, action.Rank, i+1)
		}
	}
	if !strings.Contains(rejectionReasons(review), "more than 7 actions were proposed") {
		t.Errorf("rejections = %s, want the cap refusal", rejectionReasons(review))
	}
}

// TestGroundFallsBackToADeterministicSummary makes sure a silent model still
// produces something a human can read.
func TestGroundFallsBackToADeterministicSummary(t *testing.T) {
	review := grounded(t, "overdue", ModelReview{})

	if review.Summary == "" {
		t.Fatal("summary is empty")
	}
	if !strings.Contains(review.Summary, "1 blocker(s) derived for borealis-migration at version 4") {
		t.Errorf("summary = %q, want the deterministic fallback", review.Summary)
	}
	if !strings.Contains(review.Summary, "1 critical") {
		t.Errorf("summary = %q, want the severity counts", review.Summary)
	}
	if review.NextActions == nil && review.Findings.Blockers == nil {
		t.Error("the review returned nothing at all for a project with findings")
	}
	if len(review.Findings.Overdue) != 1 {
		t.Errorf("overdue items = %+v, want the derived one", review.Findings.Overdue)
	}
}

func TestGroundSaysSoWhenThereIsNothingToReport(t *testing.T) {
	review := grounded(t, "empty", ModelReview{})
	if !strings.Contains(review.Summary, "No blockers were derived for fjord-discovery at version 1") {
		t.Errorf("summary = %q, want the empty-state summary", review.Summary)
	}
	if review.Findings.Health != HealthOnTrack {
		t.Errorf("health = %q, want on_track", review.Findings.Health)
	}
}

// TestGroundPassesChangesThroughTheDryRunGate covers the second half of the
// gate: the change set of one milestone is decided as a unit, a set that would
// leave the milestone contradictory is refused entirely, and an admissible set
// leaves as a dry run with nothing written.
func TestGroundPassesChangesThroughTheDryRunGate(t *testing.T) {
	refused := grounded(t, "missing-owner", ModelReview{
		Summary: "Nobody owns the KyB checklist.",
		Changes: []ModelChange{
			{MilestoneID: "m-1", Field: "owner_id", Value: "u-sofia"},
			{MilestoneID: "m-1", Field: "status", Value: "done"},
			{MilestoneID: "m-1", Field: "due_date", Value: "next friday"},
		},
	})
	if len(refused.Proposals) != 0 {
		t.Fatalf("proposals = %+v, want none: the set would leave m-1 done at 30%%", refused.Proposals)
	}
	reasons := rejectionReasons(refused)
	if !strings.Contains(reasons, "status done would contradict progress_percent=30") {
		t.Errorf("rejections = %s, want the contradiction refusal", reasons)
	}
	if !strings.Contains(reasons, "neither RFC3339 nor YYYY-MM-DD") {
		t.Errorf("rejections = %s, want the unparsable due date refusal", reasons)
	}
	if len(refused.Findings.Blockers) != 1 {
		t.Errorf("findings = %d blockers, want the derived one", len(refused.Findings.Blockers))
	}

	admitted := grounded(t, "missing-owner", ModelReview{
		Summary: "Nobody owns the KyB checklist.",
		Changes: []ModelChange{{MilestoneID: "m-1", Field: "owner_id", Value: "u-sofia"}},
	})
	if len(admitted.Proposals) != 1 {
		t.Fatalf("proposals = %+v, want one dry-run proposal", admitted.Proposals)
	}
	proposal := admitted.Proposals[0]
	if !proposal.DryRun {
		t.Error("the proposal is not a dry run")
	}
	if proposal.ExpectedVersion != 3 {
		t.Errorf("expected_version = %d, want 3", proposal.ExpectedVersion)
	}
	if len(proposal.Changes) != 1 || proposal.Changes[0].After != "u-sofia" {
		t.Errorf("changes = %+v, want the owner change", proposal.Changes)
	}
	if len(admitted.Rejections) != 0 {
		t.Errorf("rejections = %s, want none", rejectionReasons(admitted))
	}
}

func TestGroundReportsAnEmptyChangeSetAsNoChange(t *testing.T) {
	review := grounded(t, "missing-owner", ModelReview{Changes: nil})
	if len(review.Proposals) != 0 {
		t.Errorf("proposals = %+v, want none", review.Proposals)
	}
	if len(review.Rejections) != 0 {
		t.Errorf("rejections = %s, want none for a review that proposed nothing", rejectionReasons(review))
	}
}
