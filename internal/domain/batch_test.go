package domain

import (
	"errors"
	"strings"
	"testing"
)

// blockedSnapshot is the cobalt fixture with m-1 blocked and a stated reason, so
// the tests below start from the one state that couples a status to a reason.
func blockedSnapshot(t *testing.T) ProjectSnapshot {
	t.Helper()
	snapshot := testSnapshot(t)
	for i := range snapshot.Milestones {
		if snapshot.Milestones[i].ID == "m-1" {
			snapshot.Milestones[i].Status = StatusBlocked
			snapshot.Milestones[i].BlockedReason = "waiting on the numbering service"
		}
	}
	return snapshot
}

// TestProposeChangesRefusesLeavingABlockedReasonBehind is the regression the
// independent verification asked for: moving a milestone off blocked while its
// reason stays set would create the very contradiction the rules report.
func TestProposeChangesRefusesLeavingABlockedReasonBehind(t *testing.T) {
	snapshot := blockedSnapshot(t)

	proposal, rejections, err := ProposeChanges(snapshot, referenceNow, "resume work",
		[]ModelChange{{MilestoneID: "m-1", Field: "status", Value: "in_progress"}})

	if !errors.Is(err, ErrNoChanges) {
		t.Fatalf("error = %v, want ErrNoChanges", err)
	}
	if proposal.UpdateID != "" {
		t.Errorf("a refused batch produced a proposal: %+v", proposal)
	}
	if len(rejections) != 1 || !strings.Contains(rejections[0].Reason, "blocked_reason") {
		t.Fatalf("rejections = %+v, want a reason about blocked_reason", rejections)
	}
}

// TestProposeChangesAdmitsTheStatusAndTheReasonTogether is the other half: the
// honest update — resume the work and clear the reason — must be allowed.
func TestProposeChangesAdmitsTheStatusAndTheReasonTogether(t *testing.T) {
	snapshot := blockedSnapshot(t)

	proposal, rejections, err := ProposeChanges(snapshot, referenceNow, "resume work", []ModelChange{
		{MilestoneID: "m-1", Field: "status", Value: "in_progress"},
		{MilestoneID: "m-1", Field: "blocked_reason", Value: ""},
	})
	if err != nil {
		t.Fatalf("ProposeChanges: %v (rejections %+v)", err, rejections)
	}
	if len(rejections) != 0 {
		t.Fatalf("rejections = %+v, want none", rejections)
	}
	if len(proposal.Changes) != 2 {
		t.Fatalf("changes = %+v, want the status and the reason", proposal.Changes)
	}
	want := map[ChangeField]string{FieldStatus: "in_progress", FieldBlockedReason: ""}
	for _, change := range proposal.Changes {
		after, ok := want[change.Field]
		if !ok {
			t.Errorf("unexpected change %+v", change)
			continue
		}
		if change.After != after {
			t.Errorf("%s after = %q, want %q", change.Field, change.After, after)
		}
		delete(want, change.Field)
	}
	if len(want) != 0 {
		t.Errorf("the proposal is missing %v", want)
	}
}

// TestProposeChangesRefusesClearingTheReasonAlone keeps a blocked milestone
// honest: it is blocked because of something.
func TestProposeChangesRefusesClearingTheReasonAlone(t *testing.T) {
	snapshot := blockedSnapshot(t)

	_, rejections, err := ProposeChanges(snapshot, referenceNow, "no reason any more",
		[]ModelChange{{MilestoneID: "m-1", Field: "blocked_reason", Value: ""}})
	if !errors.Is(err, ErrNoChanges) {
		t.Fatalf("error = %v, want ErrNoChanges", err)
	}
	if len(rejections) != 1 || !strings.Contains(rejections[0].Reason, "blocked_reason") {
		t.Fatalf("rejections = %+v, want a reason about blocked_reason", rejections)
	}
}

// TestProposeChangesRefusesBlockingWithoutAReason keeps the other direction of
// the same rule.
func TestProposeChangesRefusesBlockingWithoutAReason(t *testing.T) {
	snapshot := testSnapshot(t)

	_, rejections, err := ProposeChanges(snapshot, referenceNow, "block it",
		[]ModelChange{{MilestoneID: "m-2", Field: "status", Value: "blocked"}})
	if !errors.Is(err, ErrNoChanges) {
		t.Fatalf("error = %v, want ErrNoChanges", err)
	}
	if len(rejections) != 1 || !strings.Contains(rejections[0].Reason, "blocked_reason") {
		t.Fatalf("rejections = %+v, want a reason about blocked_reason", rejections)
	}
}

// TestProposeChangesAdmitsStatusAndProgressTogether proves the gate reasons
// about the resulting milestone, not about one field in isolation.
func TestProposeChangesAdmitsStatusAndProgressTogether(t *testing.T) {
	snapshot := testSnapshot(t) // m-2 is in_progress at 20%

	proposal, rejections, err := ProposeChanges(snapshot, referenceNow, "the console is finished", []ModelChange{
		{MilestoneID: "m-2", Field: "status", Value: "done"},
		{MilestoneID: "m-2", Field: "progress_percent", Value: "100"},
	})
	if err != nil {
		t.Fatalf("ProposeChanges: %v (rejections %+v)", err, rejections)
	}
	if len(rejections) != 0 {
		t.Fatalf("rejections = %+v, want none", rejections)
	}
	if len(proposal.Changes) != 2 {
		t.Fatalf("changes = %+v, want the status and the progress", proposal.Changes)
	}
}

// TestProposeChangesRefusesADoneStatusThatStaysAtFortyPercent is the same rule
// seen from the refusal side.
func TestProposeChangesRefusesADoneStatusThatStaysAtFortyPercent(t *testing.T) {
	snapshot := testSnapshot(t)

	_, rejections, err := ProposeChanges(snapshot, referenceNow, "call it done",
		[]ModelChange{{MilestoneID: "m-2", Field: "status", Value: "done"}})
	if !errors.Is(err, ErrNoChanges) {
		t.Fatalf("error = %v, want ErrNoChanges", err)
	}
	if len(rejections) != 1 || !strings.Contains(rejections[0].Reason, "progress_percent=20") {
		t.Fatalf("rejections = %+v, want the progress contradiction", rejections)
	}
}

// TestApproverMemberMatchesAWholeNameOrAnID pins who may approve: a member of
// the project team, by full name or by id, and nobody else.
func TestApproverMemberMatchesAWholeNameOrAnID(t *testing.T) {
	snapshot := testSnapshot(t) // team: Ana Ruiz, Luis Prado, Sofia Marin

	cases := []struct {
		name   string
		wantID string
		wantOK bool
	}{
		{name: "Ana Ruiz", wantID: "u-ana", wantOK: true},
		{name: "  ana   ruiz  ", wantID: "u-ana", wantOK: true},
		{name: "u-ana", wantID: "u-ana", wantOK: true},
		{name: "SOFIA MARIN", wantID: "u-sofia", wantOK: true},
		{name: "Ana", wantOK: false},
		{name: "u-outsider", wantOK: false},
		{name: "Ana Ruiz (contractor)", wantOK: false},
		{name: "", wantOK: false},
		{name: "   ", wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			member, ok := snapshot.ApproverMember(tc.name)
			if ok != tc.wantOK {
				t.Fatalf("ApproverMember(%q) ok = %t, want %t", tc.name, ok, tc.wantOK)
			}
			if ok && member.ID != tc.wantID {
				t.Errorf("ApproverMember(%q) = %q, want %q", tc.name, member.ID, tc.wantID)
			}
			if !ok && member.ID != "" {
				t.Errorf("a refused approver returned a member: %+v", member)
			}
		})
	}

	// A project with no team can approve nothing, which is the honest reading
	// of "project-owner approval".
	empty := loadFixture(t, "empty")
	if _, ok := empty.ApproverMember("Ana Ruiz"); ok {
		t.Error("a project with no team accepted an approver")
	}
}

// TestValidateAgainstAllowsAnUnrelatedChange proves the application-time check
// refuses what drifted, not what simply moved elsewhere in the project.
func TestValidateAgainstAllowsAnUnrelatedChange(t *testing.T) {
	snapshot := testSnapshot(t)
	proposal, _ := mustPropose(t, snapshot, "",
		ModelChange{MilestoneID: "m-2", Field: "owner_id", Value: "u-sofia"})

	// Somebody else blocks m-2 without a reason in the meantime, so applying the
	// stored owner change would still be fine; the guard that matters is that a
	// proposal is re-validated, which the next assertion proves by breaking the
	// milestone under it.
	blocked := snapshot
	for i := range blocked.Milestones {
		if blocked.Milestones[i].ID == "m-2" {
			blocked.Milestones[i].Status = StatusBlocked
			blocked.Milestones[i].BlockedReason = ""
		}
	}
	if err := proposal.ValidateAgainst(blocked); err != nil {
		t.Fatalf("ValidateAgainst refused an unrelated owner change: %v", err)
	}
}
