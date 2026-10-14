package domain

import (
	"errors"
	"strings"
	"testing"
)

// testSnapshot is the working fixture for the update gate: cobalt-billing at
// version 11, with Ana owning m-2 and Sofia available on the team.
func testSnapshot(t *testing.T) ProjectSnapshot {
	t.Helper()
	return loadFixture(t, "blocked")
}

func mustPropose(t *testing.T, snapshot ProjectSnapshot, rationale string, changes ...ModelChange) (ProposedUpdate, []Rejection) {
	t.Helper()
	proposal, rejections, err := ProposeChanges(snapshot, referenceNow, rationale, changes)
	if err != nil {
		t.Fatalf("ProposeChanges: %v (rejections: %+v)", err, rejections)
	}
	return proposal, rejections
}

func TestProposeChangesAdmitsAnAllowedChange(t *testing.T) {
	snapshot := testSnapshot(t)
	proposal, rejections := mustPropose(t, snapshot, "give the console a second owner",
		ModelChange{MilestoneID: "m-2", Field: "owner_id", Value: "u-sofia"})

	if len(rejections) != 0 {
		t.Fatalf("rejections = %+v, want none", rejections)
	}
	if !proposal.DryRun {
		t.Error("the proposal is not marked as a dry run")
	}
	if proposal.ExpectedVersion != snapshot.Version {
		t.Errorf("expected_version = %d, want %d", proposal.ExpectedVersion, snapshot.Version)
	}
	if proposal.ProjectID != snapshot.ProjectID {
		t.Errorf("project_id = %q, want %q", proposal.ProjectID, snapshot.ProjectID)
	}
	if !proposal.CreatedAt.Equal(referenceNow) {
		t.Errorf("created_at = %s, want %s", proposal.CreatedAt, referenceNow)
	}
	if !strings.HasPrefix(proposal.UpdateID, "upd-") || len(proposal.UpdateID) != len("upd-")+16 {
		t.Errorf("update_id = %q, want an upd- prefix and 16 hex digits", proposal.UpdateID)
	}
	want := []Change{{MilestoneID: "m-2", Field: FieldOwnerID, Before: "u-ana", After: "u-sofia"}}
	if len(proposal.Changes) != 1 || proposal.Changes[0] != want[0] {
		t.Errorf("changes = %+v, want %+v", proposal.Changes, want)
	}
	if proposal.Rationale != "give the console a second owner" {
		t.Errorf("rationale = %q", proposal.Rationale)
	}
}

func TestProposeChangesRefusesEachInvalidIntention(t *testing.T) {
	cases := []struct {
		name    string
		change  ModelChange
		wantErr string
	}{
		{
			name:    "unknown milestone",
			change:  ModelChange{MilestoneID: "m-99", Field: "status", Value: "blocked"},
			wantErr: `unknown milestone "m-99"`,
		},
		{
			name:    "field outside the allowlist",
			change:  ModelChange{MilestoneID: "m-2", Field: "budget", Value: "1000"},
			wantErr: `field "budget" is not one of the allowlisted fields`,
		},
		{
			name:    "assigning somebody who is not on the team",
			change:  ModelChange{MilestoneID: "m-2", Field: "owner_id", Value: "u-outsider"},
			wantErr: `owner_id "u-outsider" is not on the project team`,
		},
		{
			name:    "unassigning",
			change:  ModelChange{MilestoneID: "m-2", Field: "owner_id", Value: ""},
			wantErr: "owner_id is empty",
		},
		{
			name:    "a change that changes nothing",
			change:  ModelChange{MilestoneID: "m-2", Field: "owner_id", Value: "u-ana"},
			wantErr: `owner_id already holds "u-ana"`,
		},
		{
			name:    "an unknown status",
			change:  ModelChange{MilestoneID: "m-2", Field: "status", Value: "paused"},
			wantErr: `status "paused" is not one of`,
		},
		{
			name:    "done without full progress",
			change:  ModelChange{MilestoneID: "m-2", Field: "status", Value: "done"},
			wantErr: "status done would contradict progress_percent=20",
		},
		{
			name:    "full progress without done",
			change:  ModelChange{MilestoneID: "m-2", Field: "progress_percent", Value: "100"},
			wantErr: "progress_percent 100 contradicts status in_progress",
		},
		{
			name:    "progress outside the range",
			change:  ModelChange{MilestoneID: "m-2", Field: "progress_percent", Value: "140"},
			wantErr: "outside 0..100",
		},
		{
			name:    "progress that is not a number",
			change:  ModelChange{MilestoneID: "m-2", Field: "progress_percent", Value: "half"},
			wantErr: `progress_percent "half" is not a number`,
		},
		{
			name:    "a due date before the project's last update",
			change:  ModelChange{MilestoneID: "m-2", Field: "due_date", Value: "2026-10-01"},
			wantErr: "is not after the project's last update",
		},
		{
			name:    "a due date that is not a date",
			change:  ModelChange{MilestoneID: "m-2", Field: "due_date", Value: "next friday"},
			wantErr: "neither RFC3339 nor YYYY-MM-DD",
		},
		{
			name:    "a blocked reason on a milestone that is not blocked",
			change:  ModelChange{MilestoneID: "m-2", Field: "blocked_reason", Value: "waiting"},
			wantErr: "only belongs on a blocked milestone",
		},
		{
			name:    "an empty blocked reason",
			change:  ModelChange{MilestoneID: "m-1", Field: "blocked_reason", Value: "  "},
			wantErr: `blocked_reason already holds ""`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := testSnapshot(t)
			proposal, rejections, err := ProposeChanges(snapshot, referenceNow, "", []ModelChange{tc.change})

			if !errors.Is(err, ErrNoChanges) {
				t.Fatalf("error = %v, want ErrNoChanges", err)
			}
			if proposal.UpdateID != "" || len(proposal.Changes) != 0 {
				t.Errorf("a refused change still produced a proposal: %+v", proposal)
			}
			if len(rejections) != 1 {
				t.Fatalf("rejections = %+v, want exactly one", rejections)
			}
			if rejections[0].Kind != "change" {
				t.Errorf("rejection kind = %q, want change", rejections[0].Kind)
			}
			if !strings.Contains(rejections[0].Reason, tc.wantErr) {
				t.Errorf("rejection reason = %q, want it to contain %q", rejections[0].Reason, tc.wantErr)
			}
		})
	}
}

func TestProposeChangesRefusesUpdatesToAClosedMilestone(t *testing.T) {
	snapshot := loadFixture(t, "healthy") // m-1 is done
	_, rejections, err := ProposeChanges(snapshot, referenceNow, "", []ModelChange{
		{MilestoneID: "m-1", Field: "owner_id", Value: "u-ana"},
	})
	if !errors.Is(err, ErrNoChanges) {
		t.Fatalf("error = %v, want ErrNoChanges", err)
	}
	if len(rejections) != 1 || !strings.Contains(rejections[0].Reason, "closed milestones do not accept updates") {
		t.Fatalf("rejections = %+v, want the closed-milestone refusal", rejections)
	}
}

// TestProposeChangesKeepsValidChangesAndReportsTheRest proves the gate is
// per-intention across milestones: one bad change never discards a valid one,
// while a change set for a single milestone is decided as a unit.
func TestProposeChangesKeepsValidChangesAndReportsTheRest(t *testing.T) {
	snapshot := testSnapshot(t)
	proposal, rejections, err := ProposeChanges(snapshot, referenceNow, "", []ModelChange{
		{MilestoneID: "m-2", Field: "owner_id", Value: "u-sofia"},
		{MilestoneID: "m-2", Field: "budget", Value: "1000"},
		{MilestoneID: "m-2", Field: "owner_id", Value: "u-luis"},
		{MilestoneID: "m-1", Field: "due_date", Value: "2026-11-01"},
	})
	if err != nil {
		t.Fatalf("ProposeChanges: %v", err)
	}
	if len(proposal.Changes) != 2 {
		t.Fatalf("changes = %+v, want the two valid ones", proposal.Changes)
	}
	// The canonical order sorts milestones first.
	if proposal.Changes[0].MilestoneID != "m-1" || proposal.Changes[0].Field != FieldDueDate {
		t.Errorf("first change = %+v, want m-1 due_date", proposal.Changes[0])
	}
	if proposal.Changes[1].MilestoneID != "m-2" || proposal.Changes[1].After != "u-sofia" {
		t.Errorf("second change = %+v, want the first owner change of m-2", proposal.Changes[1])
	}
	if len(rejections) != 2 {
		t.Fatalf("rejections = %+v, want two", rejections)
	}
	bySuffix := map[string]string{}
	for _, rejection := range rejections {
		bySuffix[rejection.Subject] = rejection.Reason
	}
	if reason := bySuffix["m-2/budget"]; !strings.Contains(reason, "not one of the allowlisted fields") {
		t.Errorf("budget rejection = %q", reason)
	}
	if reason := bySuffix["m-2/owner_id"]; !strings.Contains(reason, "proposed twice") {
		t.Errorf("owner_id rejection = %q, want the duplicate refusal", reason)
	}
}

// TestProposeChangesAppliesStatusAndReasonTogether covers the pair a real
// review needs: block a milestone and say why, in one update.
func TestProposeChangesAppliesStatusAndReasonTogether(t *testing.T) {
	snapshot := testSnapshot(t)
	proposal, rejections := mustPropose(t, snapshot, "record the real blockage",
		ModelChange{MilestoneID: "m-2", Field: "blocked_reason", Value: "waiting on the numbering service"},
		ModelChange{MilestoneID: "m-2", Field: "status", Value: "blocked"},
	)

	if len(rejections) != 0 {
		t.Fatalf("rejections = %+v, want none", rejections)
	}
	if len(proposal.Changes) != 2 {
		t.Fatalf("changes = %+v, want two", proposal.Changes)
	}
	// The canonical order validates and applies status before its reason.
	if proposal.Changes[0].Field != FieldStatus || proposal.Changes[1].Field != FieldBlockedReason {
		t.Errorf("change order = %s, %s; want status before blocked_reason", proposal.Changes[0].Field, proposal.Changes[1].Field)
	}
}

// TestUpdateIDDependsOnTheMutationOnly is the idempotency contract (S11): the
// same mutation against the same version is always the same update.
func TestUpdateIDDependsOnTheMutationOnly(t *testing.T) {
	snapshot := testSnapshot(t)
	change := ModelChange{MilestoneID: "m-2", Field: "owner_id", Value: "u-sofia"}

	first, _ := mustPropose(t, snapshot, "first wording", change)
	second, _ := mustPropose(t, snapshot, "a completely different rationale", change)
	if first.UpdateID != second.UpdateID {
		t.Errorf("update ids differ for the same mutation: %q vs %q", first.UpdateID, second.UpdateID)
	}

	other, otherRejections := mustPropose(t, snapshot, "other owner",
		ModelChange{MilestoneID: "m-2", Field: "owner_id", Value: "u-sofia"},
		ModelChange{MilestoneID: "m-1", Field: "status", Value: "blocked"},
		ModelChange{MilestoneID: "m-1", Field: "blocked_reason", Value: "waiting on the vendor"})
	if len(otherRejections) != 0 {
		t.Fatalf("the larger change set was refused: %+v", otherRejections)
	}
	if other.UpdateID == first.UpdateID {
		t.Error("a larger change set produced the same update id")
	}

	bumped := snapshot
	bumped.Version++
	later, _ := mustPropose(t, bumped, "first wording", change)
	if later.UpdateID == first.UpdateID {
		t.Error("the same change against a different version produced the same update id")
	}
}

func TestValidateAgainstAcceptsTheUnchangedProposal(t *testing.T) {
	snapshot := testSnapshot(t)
	proposal, _ := mustPropose(t, snapshot, "", ModelChange{MilestoneID: "m-2", Field: "owner_id", Value: "u-sofia"})

	if err := proposal.ValidateAgainst(snapshot); err != nil {
		t.Fatalf("ValidateAgainst: %v", err)
	}
}

func TestValidateAgainstRejectsAStaleVersion(t *testing.T) {
	snapshot := testSnapshot(t)
	proposal, _ := mustPropose(t, snapshot, "", ModelChange{MilestoneID: "m-2", Field: "owner_id", Value: "u-sofia"})

	moved := snapshot
	moved.Version++
	err := proposal.ValidateAgainst(moved)
	if !errors.Is(err, ErrStaleVersion) {
		t.Fatalf("error = %v, want ErrStaleVersion", err)
	}
	if !strings.Contains(err.Error(), "expected version 11, project is at 12") {
		t.Errorf("error = %v, want it to name both versions", err)
	}
}

func TestValidateAgainstRejectsADriftedField(t *testing.T) {
	snapshot := testSnapshot(t)
	proposal, _ := mustPropose(t, snapshot, "", ModelChange{MilestoneID: "m-2", Field: "owner_id", Value: "u-sofia"})

	drifted := snapshot
	for i := range drifted.Milestones {
		if drifted.Milestones[i].ID == "m-2" {
			drifted.Milestones[i].Owner = Owner{ID: "u-luis", Name: "Luis Prado"}
		}
	}
	err := proposal.ValidateAgainst(drifted)
	if !errors.Is(err, ErrDriftedChange) {
		t.Fatalf("error = %v, want ErrDriftedChange", err)
	}
	if !strings.Contains(err.Error(), `is "u-luis", the update expected "u-ana"`) {
		t.Errorf("error = %v, want it to name both values", err)
	}
}

func TestValidateAgainstRejectsADeletedMilestone(t *testing.T) {
	snapshot := testSnapshot(t)
	proposal, _ := mustPropose(t, snapshot, "", ModelChange{MilestoneID: "m-2", Field: "owner_id", Value: "u-sofia"})

	shrunk := snapshot
	shrunk.Milestones = []Milestone{snapshot.Milestones[0]}
	err := proposal.ValidateAgainst(shrunk)
	if !errors.Is(err, ErrDriftedChange) {
		t.Fatalf("error = %v, want ErrDriftedChange", err)
	}
	if !strings.Contains(err.Error(), `milestone "m-2" no longer exists`) {
		t.Errorf("error = %v, want it to name the missing milestone", err)
	}
}

func TestApplyBumpsTheVersionAndResolvesTheOwnerName(t *testing.T) {
	snapshot := testSnapshot(t)
	proposal, _ := mustPropose(t, snapshot, "", ModelChange{MilestoneID: "m-2", Field: "owner_id", Value: "u-sofia"})

	applied, err := snapshot.Apply(proposal.Changes, referenceNow)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if applied.Version != snapshot.Version+1 {
		t.Errorf("version = %d, want %d", applied.Version, snapshot.Version+1)
	}
	if !applied.UpdatedAt.Equal(referenceNow) {
		t.Errorf("updated_at = %s, want %s", applied.UpdatedAt, referenceNow)
	}
	milestone, ok := applied.Milestone("m-2")
	if !ok {
		t.Fatal("m-2 disappeared from the applied snapshot")
	}
	if milestone.Owner.ID != "u-sofia" || milestone.Owner.Name != "Sofia Marin" {
		t.Errorf("owner = %+v, want the roster entry for u-sofia", milestone.Owner)
	}
	// The original snapshot is untouched: Apply is a pure function.
	if original, _ := snapshot.Milestone("m-2"); original.Owner.ID != "u-ana" {
		t.Errorf("Apply mutated the input snapshot: %+v", original.Owner)
	}
}

func TestApprovalRequiresANameAndAMoment(t *testing.T) {
	cases := []struct {
		name     string
		approval Approval
		wantOK   bool
	}{
		{name: "named and dated", approval: Approval{By: "ana", At: referenceNow}, wantOK: true},
		{name: "trimmed", approval: Approval{By: "  ana  ", At: referenceNow}, wantOK: true},
		{name: "empty", approval: Approval{At: referenceNow}, wantOK: false},
		{name: "one character", approval: Approval{By: "a", At: referenceNow}, wantOK: false},
		{name: "whitespace only", approval: Approval{By: "   ", At: referenceNow}, wantOK: false},
		{name: "no moment", approval: Approval{By: "ana"}, wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.approval.Validate()
			if tc.wantOK && err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if !tc.wantOK && err == nil {
				t.Fatal("Validate accepted an incomplete approval")
			}
			if tc.wantOK {
				if normalized := tc.approval.Normalized(); normalized.By != "ana" {
					t.Errorf("Normalized name = %q, want ana", normalized.By)
				}
			}
		})
	}
}

func TestProposeChangesRefusesAnEmptyIntent(t *testing.T) {
	snapshot := testSnapshot(t)
	proposal, rejections, err := ProposeChanges(snapshot, referenceNow, "nothing to do", nil)
	if !errors.Is(err, ErrNoChanges) {
		t.Fatalf("error = %v, want ErrNoChanges", err)
	}
	if len(rejections) != 0 {
		t.Errorf("rejections = %+v, want none", rejections)
	}
	if proposal.UpdateID != "" {
		t.Errorf("update_id = %q, want empty", proposal.UpdateID)
	}
}
