package domain

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// referenceNow is the instant every expectation in this package is written
// against, so no test depends on the wall clock.
var referenceNow = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

func loadFixture(t *testing.T, name string) ProjectSnapshot {
	t.Helper()
	path := filepath.Join("..", "..", "testdata", "projects", name+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	var snapshot ProjectSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatalf("decode fixture %s: %v", path, err)
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("fixture %s is not a valid snapshot: %v", name, err)
	}
	return snapshot
}

// describe renders the observable parts of a blocker: rule, milestone,
// severity and owner state.
func describe(blockers []Blocker) []string {
	out := []string{}
	for _, b := range blockers {
		out = append(out, string(b.Kind)+"/"+b.MilestoneID+"/"+string(b.Severity)+"/"+string(b.OwnerStatus))
	}
	return out
}

func describeOverdue(items []OverdueItem) []string {
	out := []string{}
	for _, item := range items {
		out = append(out, item.MilestoneID+"/"+itoa(item.DaysOverdue))
	}
	return out
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestDeriveFindsEveryFixtureState is the fixture contract (S8, S12): each
// supplied state is reported with the rule, milestone, severity and owner state
// it must produce, and nothing else.
func TestDeriveFindsEveryFixtureState(t *testing.T) {
	cases := []struct {
		fixture      string
		wantBlockers []string
		wantOverdue  []string
		wantHealth   Health
	}{
		{fixture: "healthy", wantBlockers: []string{}, wantOverdue: []string{}, wantHealth: HealthOnTrack},
		{fixture: "overdue", wantBlockers: []string{"overdue/m-1/critical/assigned"}, wantOverdue: []string{"m-1/44"}, wantHealth: HealthCritical},
		{fixture: "blocked", wantBlockers: []string{"blocked_dependency/m-2/high/assigned", "overdue/m-1/high/assigned"}, wantOverdue: []string{"m-1/14"}, wantHealth: HealthAtRisk},
		{fixture: "missing-owner", wantBlockers: []string{"missing_owner/m-1/medium/unowned"}, wantOverdue: []string{}, wantHealth: HealthAtRisk},
		{fixture: "contradictory", wantBlockers: []string{"contradictory_status/m-1/high/assigned", "contradictory_status/m-2/medium/assigned"}, wantOverdue: []string{}, wantHealth: HealthAtRisk},
		{fixture: "empty", wantBlockers: []string{}, wantOverdue: []string{}, wantHealth: HealthOnTrack},
	}

	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			snapshot := loadFixture(t, tc.fixture)
			findings, err := Derive(snapshot, referenceNow, DefaultPolicy())
			if err != nil {
				t.Fatalf("Derive: %v", err)
			}
			if got := describe(findings.Blockers); !equalStrings(got, tc.wantBlockers) {
				t.Errorf("blockers = %v, want %v", got, tc.wantBlockers)
			}
			if got := describeOverdue(findings.Overdue); !equalStrings(got, tc.wantOverdue) {
				t.Errorf("overdue = %v, want %v", got, tc.wantOverdue)
			}
			if findings.Health != tc.wantHealth {
				t.Errorf("health = %q, want %q", findings.Health, tc.wantHealth)
			}
			if findings.ProjectID != snapshot.ProjectID || findings.Version != snapshot.Version {
				t.Errorf("findings identify %q v%d, want %q v%d", findings.ProjectID, findings.Version, snapshot.ProjectID, snapshot.Version)
			}
			if !findings.GeneratedAt.Equal(referenceNow) {
				t.Errorf("generated_at = %s, want %s", findings.GeneratedAt, referenceNow)
			}
			for _, b := range findings.Blockers {
				if !b.Severity.Valid() {
					t.Errorf("blocker %s has invalid severity %q", b.ID, b.Severity)
				}
				if len(b.Evidence) == 0 {
					t.Fatalf("blocker %s carries no evidence", b.ID)
				}
				for _, evidence := range b.Evidence {
					if evidence.Rule != string(b.Kind) {
						t.Errorf("blocker %s evidence rule = %q, want %q", b.ID, evidence.Rule, b.Kind)
					}
					if evidence.ProjectID != snapshot.ProjectID {
						t.Errorf("blocker %s evidence project = %q, want %q", b.ID, evidence.ProjectID, snapshot.ProjectID)
					}
					if evidence.MilestoneID != b.MilestoneID {
						t.Errorf("blocker %s evidence milestone = %q, want %q", b.ID, evidence.MilestoneID, b.MilestoneID)
					}
					if strings.TrimSpace(evidence.Detail) == "" {
						t.Errorf("blocker %s evidence has no detail", b.ID)
					}
				}
				if b.Summary == "" || b.MilestoneName == "" {
					t.Errorf("blocker %s is missing its summary or milestone name", b.ID)
				}
				if b.OwnerStatus != b.Owner.Status() {
					t.Errorf("blocker %s owner state = %q, want %q", b.ID, b.OwnerStatus, b.Owner.Status())
				}
				if b.ID != string(b.Kind)+":"+b.MilestoneID {
					t.Errorf("blocker id %q is not kind:milestone", b.ID)
				}
			}
		})
	}
}

// TestDeriveBoundaries pins the edges of the calendar rules.
func TestDeriveBoundaries(t *testing.T) {
	policy := DefaultPolicy()
	milestone := func(dueIn time.Duration, updatedAgo time.Duration) Milestone {
		m := Milestone{
			ID:              "m-1",
			Name:            "Boundary",
			Status:          StatusInProgress,
			Owner:           Owner{ID: "u-ana", Name: "Ana Ruiz"},
			ProgressPercent: 10,
		}
		if dueIn != 0 {
			m.DueDate = referenceNow.Add(-dueIn)
		}
		if updatedAgo != 0 {
			m.LastUpdateAt = referenceNow.Add(-updatedAgo)
		}
		return m
	}

	cases := []struct {
		name         string
		milestone    Milestone
		wantBlockers []string
	}{
		{name: "due exactly now is not late", milestone: milestone(0, time.Hour), wantBlockers: []string{}},
		{name: "one hour late is medium", milestone: milestone(time.Hour, time.Hour), wantBlockers: []string{"overdue/m-1/medium/assigned"}},
		{name: "exactly seven days late is high", milestone: milestone(7*24*time.Hour, time.Hour), wantBlockers: []string{"overdue/m-1/high/assigned"}},
		{name: "exactly thirty days late is critical", milestone: milestone(30*24*time.Hour, time.Hour), wantBlockers: []string{"overdue/m-1/critical/assigned"}},
		{name: "never updated is stalled", milestone: milestone(0, 0), wantBlockers: []string{"stalled/m-1/medium/assigned"}},
		{name: "updated inside the window is not stalled", milestone: milestone(0, 13*24*time.Hour), wantBlockers: []string{}},
		{name: "updated before the window is stalled", milestone: milestone(0, 15*24*time.Hour), wantBlockers: []string{"stalled/m-1/medium/assigned"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := ProjectSnapshot{
				ProjectID:  "p-boundary",
				Name:       "Boundary project",
				Version:    1,
				UpdatedAt:  referenceNow.Add(-24 * time.Hour),
				Team:       []Owner{{ID: "u-ana", Name: "Ana Ruiz"}},
				Milestones: []Milestone{tc.milestone},
			}
			findings, err := Derive(snapshot, referenceNow, policy)
			if err != nil {
				t.Fatalf("Derive: %v", err)
			}
			if got := describe(findings.Blockers); !equalStrings(got, tc.wantBlockers) {
				t.Errorf("blockers = %v, want %v", got, tc.wantBlockers)
			}
		})
	}
}

// TestDeriveNeverReportsAClosedMilestone keeps history out of the review.
func TestDeriveNeverReportsAClosedMilestone(t *testing.T) {
	snapshot := loadFixture(t, "healthy")
	findings, err := Derive(snapshot, referenceNow, DefaultPolicy())
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	for _, b := range findings.Blockers {
		if b.MilestoneID == "m-1" {
			t.Fatalf("closed milestone m-1 was reported as %s", b.Kind)
		}
	}
	// m-1 is done and 30 days past its due date; it must not be in the overdue list.
	for _, item := range findings.Overdue {
		if item.MilestoneID == "m-1" {
			t.Fatalf("closed milestone m-1 was reported overdue by %d day(s)", item.DaysOverdue)
		}
	}
}

// TestDeriveIsDeterministic proves that the order milestones appear in the file
// cannot change the findings, so two runs over the same project agree.
func TestDeriveIsDeterministic(t *testing.T) {
	for _, name := range []string{"healthy", "overdue", "blocked", "missing-owner", "contradictory", "empty"} {
		snapshot := loadFixture(t, name)
		first, err := Derive(snapshot, referenceNow, DefaultPolicy())
		if err != nil {
			t.Fatalf("Derive(%s): %v", name, err)
		}

		reversed := snapshot
		reversed.Milestones = make([]Milestone, 0, len(snapshot.Milestones))
		for i := len(snapshot.Milestones) - 1; i >= 0; i-- {
			reversed.Milestones = append(reversed.Milestones, snapshot.Milestones[i])
		}
		second, err := Derive(reversed, referenceNow, DefaultPolicy())
		if err != nil {
			t.Fatalf("Derive(%s, reversed): %v", name, err)
		}

		if got, want := describe(second.Blockers), describe(first.Blockers); !equalStrings(got, want) {
			t.Errorf("%s: blocker order changed with file order: %v vs %v", name, got, want)
		}
	}
}

// TestDeriveRefusesAnInconsistentSnapshot fails closed instead of guessing.
func TestDeriveRefusesAnInconsistentSnapshot(t *testing.T) {
	snapshot := loadFixture(t, "blocked")
	snapshot.Milestones[1].DependsOn = []string{"m-missing"}

	findings, err := Derive(snapshot, referenceNow, DefaultPolicy())
	if err == nil {
		t.Fatal("Derive accepted a dependency on a milestone that does not exist")
	}
	if !strings.Contains(err.Error(), "unknown milestone") {
		t.Errorf("error = %v, want it to name the unknown milestone", err)
	}
	if findings.Blockers != nil || findings.Overdue != nil {
		t.Errorf("findings were returned with the error: %+v", findings)
	}
}

// TestHealthOnlyEscalatesOnRealSeverity pins the three health levels.
func TestHealthOnlyEscalatesOnRealSeverity(t *testing.T) {
	low := ProjectSnapshot{
		ProjectID: "p-low",
		Name:      "Low",
		Version:   1,
		UpdatedAt: referenceNow.Add(-24 * time.Hour),
		Milestones: []Milestone{{
			ID:              "m-1",
			Name:            "Planned without an owner",
			Status:          StatusPlanned,
			ProgressPercent: 0,
			LastUpdateAt:    referenceNow.Add(-time.Hour),
		}},
	}
	findings, err := Derive(low, referenceNow, DefaultPolicy())
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if got := describe(findings.Blockers); !equalStrings(got, []string{"missing_owner/m-1/low/unowned"}) {
		t.Fatalf("blockers = %v, want one low missing_owner", got)
	}
	if findings.Health != HealthOnTrack {
		t.Errorf("health = %q, want %q for a low-only finding", findings.Health, HealthOnTrack)
	}
}

func TestAuditDigestIsStableAndCoversTheRecord(t *testing.T) {
	record := AuditRecord{
		UpdateID:        "upd-1",
		ProjectID:       "p-1",
		Action:          AuditApplied,
		ExpectedVersion: 3,
		Changes:         []Change{{MilestoneID: "m-1", Field: FieldOwnerID, Before: "", After: "u-ana"}},
		Approval:        Approval{By: "ana", At: referenceNow},
		At:              referenceNow,
	}
	first := record.WithDigest()
	if first.Digest == "" || !strings.HasPrefix(first.Digest, "sha256:") {
		t.Fatalf("digest = %q, want a sha256: prefix", first.Digest)
	}
	if second := record.WithDigest(); second.Digest != first.Digest {
		t.Errorf("digest is not stable: %q vs %q", second.Digest, first.Digest)
	}
	tampered := record
	tampered.Action = AuditDeclined
	if tampered.WithDigest().Digest == first.Digest {
		t.Error("the digest does not cover the record's action")
	}
}
