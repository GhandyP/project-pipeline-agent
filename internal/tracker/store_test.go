package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GhandyP/project-pipeline-agent/internal/domain"
)

var fixedNow = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

type fixedClock struct{ at time.Time }

func (c fixedClock) Now() time.Time { return c.at }

// newStore opens a store over a temporary directory seeded with the project
// fixtures, so a test never depends on the developer's data directory.
func newStore(t *testing.T) *Store {
	t.Helper()
	store, err := New(t.TempDir(), fixedClock{at: fixedNow})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	seeded, err := store.Seed(filepath.Join("..", "..", "testdata", "projects"))
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}
	if seeded != 6 {
		t.Fatalf("seeded %d projects, want 6", seeded)
	}
	return store
}

func mustProposal(t *testing.T, store *Store, projectID, rationale string, changes ...domain.ModelChange) domain.ProposedUpdate {
	t.Helper()
	snapshot, err := store.Snapshot(context.Background(), projectID)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	proposal, rejections, err := domain.ProposeChanges(snapshot, fixedNow, rationale, changes)
	if err != nil {
		t.Fatalf("ProposeChanges: %v (rejections %+v)", err, rejections)
	}
	if err := store.SaveProposal(context.Background(), proposal); err != nil {
		t.Fatalf("SaveProposal: %v", err)
	}
	return proposal
}

func projectVersion(t *testing.T, store *Store, projectID string) int64 {
	t.Helper()
	snapshot, err := store.Snapshot(context.Background(), projectID)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	return snapshot.Version
}

func auditActions(t *testing.T, store *Store, projectID string) []string {
	t.Helper()
	records, err := store.Audit(context.Background(), projectID)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	var out []string
	for _, record := range records {
		out = append(out, record.Action+"/"+record.UpdateID)
	}
	return out
}

func TestSeedDoesNotOverwriteAnExistingProject(t *testing.T) {
	store := newStore(t)
	again, err := store.Seed(filepath.Join("..", "..", "testdata", "projects"))
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}
	if again != 0 {
		t.Errorf("second Seed copied %d projects, want 0", again)
	}
}

func TestListProjects(t *testing.T) {
	store := newStore(t)
	summaries, err := store.ListProjects(context.Background())
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	var ids []string
	for _, summary := range summaries {
		ids = append(ids, summary.ProjectID)
	}
	want := []string{"atlas-checkout", "borealis-migration", "cobalt-billing", "delta-onboarding", "ember-reporting", "fjord-discovery"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Errorf("projects = %v, want %v", ids, want)
	}
	blocked := summaries[2]
	if blocked.ProjectID != "cobalt-billing" || blocked.Version != 11 || blocked.MilestoneCount != 2 || blocked.OpenCount != 2 {
		t.Errorf("summary = %+v, want cobalt-billing v11 with 2 open of 2", blocked)
	}
}

func TestSnapshotRefusesAMissingProject(t *testing.T) {
	store := newStore(t)
	_, err := store.Snapshot(context.Background(), "no-such-project")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

// TestApplyWritesOnceAndRecordsBothSteps is the happy path end to end.
func TestApplyWritesOnceAndRecordsBothSteps(t *testing.T) {
	store := newStore(t)
	proposal := mustProposal(t, store, "cobalt-billing", "give the console a second owner",
		domain.ModelChange{MilestoneID: "m-2", Field: "owner_id", Value: "u-sofia"})

	if got := auditActions(t, store, "cobalt-billing"); len(got) != 1 || !strings.HasPrefix(got[0], domain.AuditProposed) {
		t.Fatalf("after proposing, audit = %v, want one proposed record", got)
	}

	applied, err := store.Apply(context.Background(), "cobalt-billing", proposal.UpdateID, domain.Approval{By: "ana", At: fixedNow})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if applied.ExpectedVersion != 11 || applied.ResultingVersion != 12 {
		t.Errorf("versions = %d -> %d, want 11 -> 12", applied.ExpectedVersion, applied.ResultingVersion)
	}
	if applied.Approval.By != "ana" || !applied.AppliedAt.Equal(fixedNow) {
		t.Errorf("approval = %+v, want ana at %s", applied.Approval, fixedNow)
	}
	if got := projectVersion(t, store, "cobalt-billing"); got != 12 {
		t.Errorf("stored version = %d, want 12", got)
	}

	snapshot, err := store.Snapshot(context.Background(), "cobalt-billing")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	milestone, _ := snapshot.Milestone("m-2")
	if milestone.Owner.ID != "u-sofia" || milestone.Owner.Name != "Sofia Marin" {
		t.Errorf("owner = %+v, want Sofia Marin from the roster", milestone.Owner)
	}
	if !snapshot.UpdatedAt.Equal(fixedNow) {
		t.Errorf("updated_at = %s, want %s", snapshot.UpdatedAt, fixedNow)
	}

	records, err := store.Audit(context.Background(), "cobalt-billing")
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("audit has %d records, want proposed + applied", len(records))
	}
	last := records[1]
	if last.Action != domain.AuditApplied || last.ResultingVersion != 12 || last.Approval.By != "ana" {
		t.Errorf("applied record = %+v", last)
	}
	if last.Digest == "" || !strings.HasPrefix(last.Digest, "sha256:") {
		t.Errorf("digest = %q, want a sha256 digest", last.Digest)
	}
	for _, record := range records {
		if !record.At.Equal(fixedNow) {
			t.Errorf("record %s at %s, want %s", record.Action, record.At, fixedNow)
		}
	}
}

// TestApplyReplayReturnsTheRecordedOutcome is the idempotency contract (S11):
// the second identical decision changes nothing.
func TestApplyReplayReturnsTheRecordedOutcome(t *testing.T) {
	store := newStore(t)
	proposal := mustProposal(t, store, "cobalt-billing", "",
		domain.ModelChange{MilestoneID: "m-2", Field: "owner_id", Value: "u-sofia"})
	approval := domain.Approval{By: "ana", At: fixedNow}

	first, err := store.Apply(context.Background(), "cobalt-billing", proposal.UpdateID, approval)
	if err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	second, err := store.Apply(context.Background(), "cobalt-billing", proposal.UpdateID, approval)
	if err != nil {
		t.Fatalf("replayed Apply: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("replay returned %+v, want the recorded %+v", second, first)
	}
	if got := projectVersion(t, store, "cobalt-billing"); got != 12 {
		t.Errorf("version after replay = %d, want 12", got)
	}
	if got := auditActions(t, store, "cobalt-billing"); len(got) != 2 {
		t.Errorf("audit = %v, want exactly two records after a replay", got)
	}
}

// TestApplyRefusesAStaleProject proves the version check happens before any
// write: a project that moved leaves no change behind.
func TestApplyRefusesAStaleProject(t *testing.T) {
	store := newStore(t)
	proposal := mustProposal(t, store, "cobalt-billing", "",
		domain.ModelChange{MilestoneID: "m-2", Field: "owner_id", Value: "u-sofia"})

	// Somebody else moves the project after the proposal was stored.
	path := filepath.Join(store.Dir(), "projects", "cobalt-billing.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read project: %v", err)
	}
	var snapshot domain.ProjectSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatalf("decode project: %v", err)
	}
	snapshot.Version = 12
	snapshot.UpdatedAt = fixedNow.Add(time.Hour)
	encoded, _ := json.MarshalIndent(snapshot, "", "  ")
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		t.Fatalf("write project: %v", err)
	}

	_, err = store.Apply(context.Background(), "cobalt-billing", proposal.UpdateID, domain.Approval{By: "ana", At: fixedNow})
	if !errors.Is(err, domain.ErrStaleVersion) {
		t.Fatalf("error = %v, want ErrStaleVersion", err)
	}
	if got := projectVersion(t, store, "cobalt-billing"); got != 12 {
		t.Errorf("version = %d, want the project to stay at 12", got)
	}
	if got := auditActions(t, store, "cobalt-billing"); len(got) != 1 {
		t.Errorf("audit = %v, want only the proposed record", got)
	}
}

func TestApplyRefusesADriftedField(t *testing.T) {
	store := newStore(t)
	proposal := mustProposal(t, store, "cobalt-billing", "",
		domain.ModelChange{MilestoneID: "m-2", Field: "owner_id", Value: "u-sofia"})

	// The same version, but somebody edited the owner without bumping it.
	path := filepath.Join(store.Dir(), "projects", "cobalt-billing.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read project: %v", err)
	}
	var snapshot domain.ProjectSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatalf("decode project: %v", err)
	}
	for i := range snapshot.Milestones {
		if snapshot.Milestones[i].ID == "m-2" {
			snapshot.Milestones[i].Owner = domain.Owner{ID: "u-luis", Name: "Luis Prado"}
		}
	}
	encoded, _ := json.MarshalIndent(snapshot, "", "  ")
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		t.Fatalf("write project: %v", err)
	}

	_, err = store.Apply(context.Background(), "cobalt-billing", proposal.UpdateID, domain.Approval{By: "ana", At: fixedNow})
	if !errors.Is(err, domain.ErrDriftedChange) {
		t.Fatalf("error = %v, want ErrDriftedChange", err)
	}
	if got := auditActions(t, store, "cobalt-billing"); len(got) != 1 {
		t.Errorf("audit = %v, want nothing recorded for a refused apply", got)
	}
}

func TestApplyRefusesAnIncompleteApproval(t *testing.T) {
	cases := []struct {
		name     string
		approval domain.Approval
	}{
		{name: "no name", approval: domain.Approval{At: fixedNow}},
		{name: "one character", approval: domain.Approval{By: "a", At: fixedNow}},
		{name: "whitespace name", approval: domain.Approval{By: "   ", At: fixedNow}},
		{name: "no moment", approval: domain.Approval{By: "ana"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newStore(t)
			proposal := mustProposal(t, store, "cobalt-billing", "",
				domain.ModelChange{MilestoneID: "m-2", Field: "owner_id", Value: "u-sofia"})

			_, err := store.Apply(context.Background(), "cobalt-billing", proposal.UpdateID, tc.approval)
			if err == nil {
				t.Fatal("Apply accepted an incomplete approval")
			}
			if got := projectVersion(t, store, "cobalt-billing"); got != 11 {
				t.Errorf("version = %d, want the project untouched at 11", got)
			}
			if got := auditActions(t, store, "cobalt-billing"); len(got) != 1 {
				t.Errorf("audit = %v, want only the proposed record", got)
			}
		})
	}
}

func TestApplyRefusesAnUnknownUpdate(t *testing.T) {
	store := newStore(t)
	_, err := store.Apply(context.Background(), "cobalt-billing", "upd-doesnotexist", domain.Approval{By: "ana", At: fixedNow})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
	if got := projectVersion(t, store, "cobalt-billing"); got != 11 {
		t.Errorf("version = %d, want 11", got)
	}
}

// TestDeclineTouchesNothingAndBlocksALaterApply covers the refusal path.
func TestDeclineTouchesNothingAndBlocksALaterApply(t *testing.T) {
	store := newStore(t)
	proposal := mustProposal(t, store, "cobalt-billing", "",
		domain.ModelChange{MilestoneID: "m-2", Field: "owner_id", Value: "u-sofia"})
	approval := domain.Approval{By: "luis", At: fixedNow, Note: "not our call"}

	record, err := store.Decline(context.Background(), "cobalt-billing", proposal.UpdateID, approval)
	if err != nil {
		t.Fatalf("Decline: %v", err)
	}
	if record.Action != domain.AuditDeclined || record.ResultingVersion != 0 {
		t.Errorf("record = %+v, want a declined record with no version bump", record)
	}
	if got := projectVersion(t, store, "cobalt-billing"); got != 11 {
		t.Errorf("version = %d, want 11", got)
	}
	if got := auditActions(t, store, "cobalt-billing"); len(got) != 2 {
		t.Fatalf("audit = %v, want proposed + declined", got)
	}

	replayed, err := store.Decline(context.Background(), "cobalt-billing", proposal.UpdateID, approval)
	if err != nil {
		t.Fatalf("replayed Decline: %v", err)
	}
	if replayed.UpdateID != record.UpdateID {
		t.Errorf("replayed decline = %+v, want the recorded one", replayed)
	}
	if got := auditActions(t, store, "cobalt-billing"); len(got) != 2 {
		t.Errorf("audit = %v, want exactly two records after a replay", got)
	}

	_, err = store.Apply(context.Background(), "cobalt-billing", proposal.UpdateID, domain.Approval{By: "ana", At: fixedNow})
	if !errors.Is(err, ErrAlreadyDecided) {
		t.Fatalf("error = %v, want ErrAlreadyDecided", err)
	}
	if got := projectVersion(t, store, "cobalt-billing"); got != 11 {
		t.Errorf("version = %d, want 11", got)
	}
}

func TestApplyRefusesAnUnknownProject(t *testing.T) {
	store := newStore(t)
	_, err := store.Apply(context.Background(), "no-such-project", "upd-1", domain.Approval{By: "ana", At: fixedNow})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

// TestIdsCannotEscapeTheStore is the path-traversal guard: an id is refused
// before it reaches the filesystem.
func TestIdsCannotEscapeTheStore(t *testing.T) {
	store := newStore(t)
	bad := []string{"../evil", "..", "a/b", ".hidden", "a b", "", strings.Repeat("x", 65)}
	for _, id := range bad {
		if _, err := store.Snapshot(context.Background(), id); !errors.Is(err, ErrInvalidID) {
			t.Errorf("Snapshot(%q) error = %v, want ErrInvalidID", id, err)
		}
		if _, err := store.Proposal(context.Background(), id, "upd-1"); !errors.Is(err, ErrInvalidID) {
			t.Errorf("Proposal(%q) error = %v, want ErrInvalidID", id, err)
		}
		if _, err := store.Proposal(context.Background(), "cobalt-billing", id); !errors.Is(err, ErrInvalidID) {
			t.Errorf("Proposal(update %q) error = %v, want ErrInvalidID", id, err)
		}
		if _, err := store.Apply(context.Background(), "cobalt-billing", id, domain.Approval{By: "ana", At: fixedNow}); !errors.Is(err, ErrInvalidID) {
			t.Errorf("Apply(update %q) error = %v, want ErrInvalidID", id, err)
		}
		if _, err := store.Audit(context.Background(), id); !errors.Is(err, ErrInvalidID) {
			t.Errorf("Audit(%q) error = %v, want ErrInvalidID", id, err)
		}
	}
	if entries, err := os.ReadDir(filepath.Join(store.Dir(), "projects")); err != nil || len(entries) != 6 {
		t.Errorf("projects directory holds %d entries (err %v), want the seeded 6", len(entries), err)
	}
}

// TestSaveProposalRefusesANonDryRunUpdate keeps the store honest about what a
// proposal is.
func TestSaveProposalRefusesANonDryRunUpdate(t *testing.T) {
	store := newStore(t)
	proposal := mustProposal(t, store, "cobalt-billing", "",
		domain.ModelChange{MilestoneID: "m-2", Field: "owner_id", Value: "u-sofia"})
	proposal.DryRun = false
	proposal.UpdateID = "upd-notdryrun"

	err := store.SaveProposal(context.Background(), proposal)
	if err == nil || !strings.Contains(err.Error(), "dry-run") {
		t.Fatalf("error = %v, want a dry-run refusal", err)
	}
	if _, err := os.Stat(filepath.Join(store.Dir(), "proposals", "cobalt-billing", "upd-notdryrun.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a non-dry-run proposal reached the filesystem: %v", err)
	}
}

// TestNoTemporaryFilesAreLeftBehind proves the atomic writer cleans up.
func TestNoTemporaryFilesAreLeftBehind(t *testing.T) {
	store := newStore(t)
	proposal := mustProposal(t, store, "cobalt-billing", "",
		domain.ModelChange{MilestoneID: "m-2", Field: "owner_id", Value: "u-sofia"})
	if _, err := store.Apply(context.Background(), "cobalt-billing", proposal.UpdateID, domain.Approval{By: "ana", At: fixedNow}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	err := filepath.WalkDir(store.Dir(), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.Contains(entry.Name(), ".tmp-") {
			t.Errorf("temporary file left behind: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

func TestSnapshotContextIsHonoured(t *testing.T) {
	store := newStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Snapshot(ctx, "cobalt-billing"); !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}
