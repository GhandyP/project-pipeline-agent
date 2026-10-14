package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GhandyP/project-pipeline-agent/internal/domain"
	"github.com/GhandyP/project-pipeline-agent/internal/ports"
)

var fixedNow = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

type fixedClock struct{ at time.Time }

func (c fixedClock) Now() time.Time { return c.at }

// fakeTracker records every call, so a test can prove that a refused decision
// reached nothing.
type fakeTracker struct {
	snapshot    domain.ProjectSnapshot
	snapshotErr error

	applied     domain.AppliedUpdate
	applyErr    error
	record      domain.AuditRecord
	declineErr  error
	proposal    domain.ProposedUpdate
	proposalErr error
	audit       []domain.AuditRecord

	snapshotCalls int
	saveCalls     int
	saved         []domain.ProposedUpdate
	applyCalls    int
	declineCalls  int
	lastApproval  domain.Approval
	lastUpdateID  string
}

func (f *fakeTracker) ListProjects(context.Context) ([]domain.ProjectSummary, error) {
	if f.snapshot.ProjectID == "" {
		return nil, nil
	}
	return []domain.ProjectSummary{f.snapshot.Summary()}, nil
}

func (f *fakeTracker) Snapshot(context.Context, string) (domain.ProjectSnapshot, error) {
	f.snapshotCalls++
	return f.snapshot, f.snapshotErr
}

func (f *fakeTracker) SaveProposal(_ context.Context, proposal domain.ProposedUpdate) error {
	f.saveCalls++
	f.saved = append(f.saved, proposal)
	return nil
}

func (f *fakeTracker) Proposal(context.Context, string, string) (domain.ProposedUpdate, error) {
	return f.proposal, f.proposalErr
}

func (f *fakeTracker) Apply(_ context.Context, _ string, updateID string, approval domain.Approval) (domain.AppliedUpdate, error) {
	f.applyCalls++
	f.lastUpdateID = updateID
	f.lastApproval = approval
	return f.applied, f.applyErr
}

func (f *fakeTracker) Decline(_ context.Context, _ string, updateID string, approval domain.Approval) (domain.AuditRecord, error) {
	f.declineCalls++
	f.lastUpdateID = updateID
	f.lastApproval = approval
	return f.record, f.declineErr
}

func (f *fakeTracker) Audit(context.Context, string) ([]domain.AuditRecord, error) {
	return f.audit, nil
}

var _ ports.Tracker = (*fakeTracker)(nil)

func loadSnapshot(t *testing.T, fixture string) domain.ProjectSnapshot {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "projects", fixture+".json"))
	if err != nil {
		t.Fatalf("read fixture %s: %v", fixture, err)
	}
	var snapshot domain.ProjectSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatalf("decode fixture %s: %v", fixture, err)
	}
	return snapshot
}

func newService(t *testing.T, tracker *fakeTracker) *Service {
	t.Helper()
	svc, err := New(tracker, fixedClock{at: fixedNow}, domain.DefaultPolicy())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc
}

func TestNewRefusesMissingDependencies(t *testing.T) {
	if _, err := New(nil, fixedClock{at: fixedNow}, domain.DefaultPolicy()); err == nil {
		t.Error("New accepted a nil tracker")
	}
	if _, err := New(&fakeTracker{}, nil, domain.DefaultPolicy()); err == nil {
		t.Error("New accepted a nil clock")
	}
}

func TestFindingsComeFromTheRules(t *testing.T) {
	tracker := &fakeTracker{snapshot: loadSnapshot(t, "blocked")}
	svc := newService(t, tracker)

	findings, err := svc.Findings(context.Background(), "cobalt-billing")
	if err != nil {
		t.Fatalf("Findings: %v", err)
	}
	if findings.Health != domain.HealthAtRisk {
		t.Errorf("health = %q, want %q", findings.Health, domain.HealthAtRisk)
	}
	var kinds []string
	for _, blocker := range findings.Blockers {
		kinds = append(kinds, string(blocker.Kind))
	}
	if strings.Join(kinds, ",") != "blocked_dependency,overdue" {
		t.Errorf("blockers = %v, want the two derived rules", kinds)
	}
	if !findings.GeneratedAt.Equal(fixedNow) {
		t.Errorf("generated_at = %s, want the injected clock %s", findings.GeneratedAt, fixedNow)
	}
	if tracker.snapshotCalls != 1 {
		t.Errorf("snapshot calls = %d, want 1", tracker.snapshotCalls)
	}
}

// TestProposeStoresADryRunAndTouchesNoProject is the write boundary: proposing
// records an intention and nothing else.
func TestProposeStoresADryRunAndTouchesNoProject(t *testing.T) {
	tracker := &fakeTracker{snapshot: loadSnapshot(t, "missing-owner")}
	svc := newService(t, tracker)

	proposal, rejections, err := svc.Propose(context.Background(), "delta-onboarding", "assign the checklist",
		[]domain.ModelChange{{MilestoneID: "m-1", Field: "owner_id", Value: "u-sofia"}})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if len(rejections) != 0 {
		t.Fatalf("rejections = %+v, want none", rejections)
	}
	if !proposal.DryRun || proposal.ExpectedVersion != 3 {
		t.Errorf("proposal = %+v, want a dry run against version 3", proposal)
	}
	if tracker.saveCalls != 1 || len(tracker.saved) != 1 || tracker.saved[0].UpdateID != proposal.UpdateID {
		t.Errorf("saved = %+v, want the one proposal", tracker.saved)
	}
	if tracker.applyCalls != 0 || tracker.declineCalls != 0 {
		t.Errorf("proposing reached apply=%d decline=%d, want none", tracker.applyCalls, tracker.declineCalls)
	}
}

func TestProposeRefusesAnEmptyIntentWithoutStoringIt(t *testing.T) {
	tracker := &fakeTracker{snapshot: loadSnapshot(t, "missing-owner")}
	svc := newService(t, tracker)

	_, _, err := svc.Propose(context.Background(), "delta-onboarding", "", nil)
	if !errors.Is(err, ErrNoChanges) {
		t.Fatalf("error = %v, want ErrNoChanges", err)
	}
	if tracker.saveCalls != 0 {
		t.Errorf("save calls = %d, want a refused proposal never to be stored", tracker.saveCalls)
	}
}

func TestProposeKeepsTheValidChangesAndReportsTheRest(t *testing.T) {
	tracker := &fakeTracker{snapshot: loadSnapshot(t, "missing-owner")}
	svc := newService(t, tracker)

	proposal, rejections, err := svc.Propose(context.Background(), "delta-onboarding", "",
		[]domain.ModelChange{
			{MilestoneID: "m-1", Field: "owner_id", Value: "u-sofia"},
			{MilestoneID: "m-1", Field: "status", Value: "paused"},
		})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if len(proposal.Changes) != 1 || proposal.Changes[0].After != "u-sofia" {
		t.Errorf("changes = %+v, want only the owner change", proposal.Changes)
	}
	if len(rejections) != 1 || !strings.Contains(rejections[0].Reason, "status \"paused\"") {
		t.Errorf("rejections = %+v, want the status refusal", rejections)
	}
}

func TestGroundKeepsTheRulesAndShowsTheRefusals(t *testing.T) {
	tracker := &fakeTracker{snapshot: loadSnapshot(t, "overdue")}
	svc := newService(t, tracker)

	review, err := svc.Ground(context.Background(), "borealis-migration", domain.ModelReview{
		Summary: "The schema freeze is late.",
		NextActions: []domain.ModelAction{
			{Action: "Escalate the freeze", BlockerIDs: []string{"overdue:m-1"}, OwnerID: "u-ana"},
			{Action: "Invent a blocker", BlockerIDs: []string{"overdue:m-9"}},
		},
	})
	if err != nil {
		t.Fatalf("Ground: %v", err)
	}
	if len(review.Findings.Blockers) != 1 || review.Findings.Blockers[0].Kind != domain.BlockerOverdue {
		t.Fatalf("findings = %+v, want the one overdue blocker", review.Findings.Blockers)
	}
	if len(review.NextActions) != 1 || review.NextActions[0].Rank != 1 {
		t.Errorf("next actions = %+v, want only the traceable one", review.NextActions)
	}
	if review.NextActions[0].Owner.ID != "u-ana" {
		t.Errorf("owner = %+v, want the roster entry", review.NextActions[0].Owner)
	}
	if len(review.Rejections) != 1 || !strings.Contains(review.Rejections[0].Reason, "overdue:m-9") {
		t.Errorf("rejections = %+v, want the invented id refused", review.Rejections)
	}
}

// TestDecideApproveCarriesTheApprovalToTheStore proves the service builds the
// approval from its own clock and its own validation.
func TestDecideApproveCarriesTheApprovalToTheStore(t *testing.T) {
	tracker := &fakeTracker{
		snapshot: loadSnapshot(t, "blocked"),
		applied: domain.AppliedUpdate{
			UpdateID:         "upd-1",
			ProjectID:        "cobalt-billing",
			ExpectedVersion:  11,
			ResultingVersion: 12,
		},
	}
	svc := newService(t, tracker)

	applied, record, err := svc.Decide(context.Background(), "cobalt-billing", "upd-1", Decision{
		Approved: true,
		By:       "  ana  ",
		Note:     "  reviewed the console plan  ",
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if applied.ResultingVersion != 12 {
		t.Errorf("applied = %+v, want the store's outcome", applied)
	}
	if record.UpdateID != "" {
		t.Errorf("record = %+v, want no record for an approval", record)
	}
	if tracker.applyCalls != 1 || tracker.declineCalls != 0 {
		t.Fatalf("apply=%d decline=%d, want exactly one apply", tracker.applyCalls, tracker.declineCalls)
	}
	if tracker.lastUpdateID != "upd-1" {
		t.Errorf("update id = %q, want upd-1", tracker.lastUpdateID)
	}
	if tracker.lastApproval.By != "ana" || tracker.lastApproval.Note != "reviewed the console plan" {
		t.Errorf("approval = %+v, want the trimmed name and note", tracker.lastApproval)
	}
	if !tracker.lastApproval.At.Equal(fixedNow) {
		t.Errorf("approval moment = %s, want the injected clock %s", tracker.lastApproval.At, fixedNow)
	}
}

func TestDecideDeclineRecordsAndNeverApplies(t *testing.T) {
	tracker := &fakeTracker{
		snapshot: loadSnapshot(t, "blocked"),
		record:   domain.AuditRecord{UpdateID: "upd-1", ProjectID: "cobalt-billing", Action: domain.AuditDeclined},
	}
	svc := newService(t, tracker)

	applied, record, err := svc.Decide(context.Background(), "cobalt-billing", "upd-1", Decision{Approved: false, By: "luis"})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if applied.UpdateID != "" {
		t.Errorf("applied = %+v, want nothing applied on a refusal", applied)
	}
	if record.Action != domain.AuditDeclined {
		t.Errorf("record = %+v, want the declined record", record)
	}
	if tracker.applyCalls != 0 || tracker.declineCalls != 1 {
		t.Errorf("apply=%d decline=%d, want a refusal to never reach apply", tracker.applyCalls, tracker.declineCalls)
	}
}

// TestDecideRefusesAnAnonymousApprovalBeforeTheStore fails closed before any
// write path is reached.
func TestDecideRefusesAnAnonymousApprovalBeforeTheStore(t *testing.T) {
	cases := []Decision{
		{Approved: true, By: ""},
		{Approved: true, By: " "},
		{Approved: true, By: "a"},
		{Approved: false, By: "a"},
	}
	for _, decision := range cases {
		tracker := &fakeTracker{snapshot: loadSnapshot(t, "blocked")}
		svc := newService(t, tracker)

		if _, _, err := svc.Decide(context.Background(), "cobalt-billing", "upd-1", decision); err == nil {
			t.Fatalf("Decide accepted %+v", decision)
		}
		if tracker.applyCalls != 0 || tracker.declineCalls != 0 {
			t.Errorf("decision %+v reached the store: apply=%d decline=%d", decision, tracker.applyCalls, tracker.declineCalls)
		}
	}
}

func TestDecidePropagatesTheStoreErrors(t *testing.T) {
	tracker := &fakeTracker{snapshot: loadSnapshot(t, "blocked"), applyErr: domain.ErrStaleVersion}
	svc := newService(t, tracker)

	_, _, err := svc.Decide(context.Background(), "cobalt-billing", "upd-1", Decision{Approved: true, By: "ana"})
	if !errors.Is(err, domain.ErrStaleVersion) {
		t.Fatalf("error = %v, want ErrStaleVersion", err)
	}
}

func TestProposePropagatesAMissingProject(t *testing.T) {
	tracker := &fakeTracker{snapshotErr: errors.New("tracker: not found")}
	svc := newService(t, tracker)

	_, _, err := svc.Propose(context.Background(), "nope", "", []domain.ModelChange{{MilestoneID: "m-1", Field: "status", Value: "blocked"}})
	if err == nil {
		t.Fatal("Propose accepted a project the store does not have")
	}
	if tracker.saveCalls != 0 {
		t.Errorf("save calls = %d, want none", tracker.saveCalls)
	}
}

func TestStoredProposalAndAuditDelegate(t *testing.T) {
	tracker := &fakeTracker{
		snapshot: loadSnapshot(t, "blocked"),
		proposal: domain.ProposedUpdate{UpdateID: "upd-1", ProjectID: "cobalt-billing", DryRun: true},
		audit:    []domain.AuditRecord{{UpdateID: "upd-1", Action: domain.AuditProposed}},
	}
	svc := newService(t, tracker)

	proposal, err := svc.StoredProposal(context.Background(), "cobalt-billing", "upd-1")
	if err != nil || proposal.UpdateID != "upd-1" {
		t.Fatalf("StoredProposal = %+v, %v", proposal, err)
	}
	records, err := svc.Audit(context.Background(), "cobalt-billing")
	if err != nil || len(records) != 1 || records[0].Action != domain.AuditProposed {
		t.Fatalf("Audit = %+v, %v", records, err)
	}
	summaries, err := svc.ListProjects(context.Background())
	if err != nil || len(summaries) != 1 || summaries[0].ProjectID != "cobalt-billing" {
		t.Fatalf("ListProjects = %+v, %v", summaries, err)
	}
}
