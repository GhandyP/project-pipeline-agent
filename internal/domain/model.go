// Package domain holds the pipeline contract and the pure rules that decide
// what is true about a project.
//
// Nothing in this package performs I/O, reads the clock implicitly, or talks to
// a model: every judgement here is a pure function of a snapshot, a policy and
// the current time, so each rule is provable in a test instead of trusted.
package domain

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Severity is how bad a finding is. Severities are never authored by a model:
// they come from the rule table in rules.go.
type Severity string

const (
	SeverityLow      Severity = "low"
	SeverityMedium   Severity = "medium"
	SeverityHigh     Severity = "high"
	SeverityCritical Severity = "critical"
)

var severityRank = map[Severity]int{
	SeverityLow:      1,
	SeverityMedium:   2,
	SeverityHigh:     3,
	SeverityCritical: 4,
}

// Rank orders severities from 1 (low) to 4 (critical). An unknown severity
// ranks 0, which sorts last.
func (s Severity) Rank() int { return severityRank[s] }

// Valid reports whether the severity is one of the four defined values.
func (s Severity) Valid() bool {
	_, ok := severityRank[s]
	return ok
}

// AllSeverities lists every severity from low to critical.
func AllSeverities() []Severity {
	return []Severity{SeverityLow, SeverityMedium, SeverityHigh, SeverityCritical}
}

// MilestoneStatus is where a milestone stands in the tracker.
type MilestoneStatus string

const (
	StatusPlanned    MilestoneStatus = "planned"
	StatusInProgress MilestoneStatus = "in_progress"
	StatusBlocked    MilestoneStatus = "blocked"
	StatusDone       MilestoneStatus = "done"
	StatusCancelled  MilestoneStatus = "cancelled"
)

var milestoneStatuses = map[MilestoneStatus]bool{
	StatusPlanned:    true,
	StatusInProgress: true,
	StatusBlocked:    true,
	StatusDone:       true,
	StatusCancelled:  true,
}

// Valid reports whether the status is one of the five defined values.
func (s MilestoneStatus) Valid() bool { return milestoneStatuses[s] }

// Open reports whether the status still needs attention. A closed milestone is
// never reported as overdue, stalled or unowned, and never accepts an update.
func (s MilestoneStatus) Open() bool { return s != StatusDone && s != StatusCancelled }

// AllMilestoneStatuses lists every status in the order the update fields use.
func AllMilestoneStatuses() []MilestoneStatus {
	return []MilestoneStatus{StatusPlanned, StatusInProgress, StatusBlocked, StatusDone, StatusCancelled}
}

// OwnerStatus is whether a milestone has somebody answerable for it.
type OwnerStatus string

const (
	OwnerAssigned OwnerStatus = "assigned"
	OwnerUnowned  OwnerStatus = "unowned"
)

// Owner is the person answerable for a milestone. An empty ID means nobody is.
type Owner struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// IsZero reports whether nobody is assigned.
func (o Owner) IsZero() bool { return strings.TrimSpace(o.ID) == "" }

// Status reports the owner state carried by every blocker (S10).
func (o Owner) Status() OwnerStatus {
	if o.IsZero() {
		return OwnerUnowned
	}
	return OwnerAssigned
}

// Milestone is one deliverable of a project.
type Milestone struct {
	ID              string          `json:"id"`
	Name            string          `json:"name"`
	Status          MilestoneStatus `json:"status"`
	Owner           Owner           `json:"owner"`
	DueDate         time.Time       `json:"due_date"`
	ProgressPercent int             `json:"progress_percent"`
	DependsOn       []string        `json:"depends_on,omitempty"`
	BlockedReason   string          `json:"blocked_reason,omitempty"`
	LastUpdateAt    time.Time       `json:"last_update_at"`
	LastUpdate      string          `json:"last_update,omitempty"`
}

// Risk is a known risk recorded in the tracker. Risks are reported as they are;
// they are not re-scored by the review.
type Risk struct {
	ID          string    `json:"id"`
	Description string    `json:"description"`
	Severity    Severity  `json:"severity"`
	Owner       Owner     `json:"owner"`
	Mitigation  string    `json:"mitigation,omitempty"`
	RaisedAt    time.Time `json:"raised_at"`
}

// ProjectSnapshot is the read-only view of one project at one tracker version,
// the input of every rule (S5).
type ProjectSnapshot struct {
	ProjectID  string      `json:"project_id"`
	Name       string      `json:"name"`
	OwnerID    string      `json:"owner_id"` // May be empty on historical snapshots; approvals then fail closed.
	Version    int64       `json:"version"`
	UpdatedAt  time.Time   `json:"updated_at"`
	Team       []Owner     `json:"team"`
	Milestones []Milestone `json:"milestones"`
	Risks      []Risk      `json:"risks,omitempty"`
}

// Milestone returns the milestone with that id.
func (s ProjectSnapshot) Milestone(id string) (Milestone, bool) {
	for _, m := range s.Milestones {
		if m.ID == id {
			return m, true
		}
	}
	return Milestone{}, false
}

// TeamMember returns the team member with that id.
func (s ProjectSnapshot) TeamMember(id string) (Owner, bool) {
	for _, member := range s.Team {
		if member.ID == id {
			return member, true
		}
	}
	return Owner{}, false
}

// ProjectOwner returns the configured owner only when OwnerID identifies one
// unambiguous member of the project roster. An unset or stale owner id fails
// closed without making the snapshot unreadable.
func (s ProjectSnapshot) ProjectOwner() (Owner, bool) {
	want := normalizePerson(s.OwnerID)
	if want == "" {
		return Owner{}, false
	}

	var owner Owner
	found := false
	for _, member := range s.Team {
		if normalizePerson(member.ID) != want {
			continue
		}
		if found {
			return Owner{}, false
		}
		owner, found = member, true
	}
	return owner, found
}

// ApproverMember reports whether name identifies the configured project owner
// by id or full name. Matching ignores case and repeated spaces because a
// human types it. An absent or unrecognized owner id is not approval authority.
// The name is accountability, not authentication: nothing here proves who
// typed it, which is why docs/missing-work.md lists identity as an open gap.
func (s ProjectSnapshot) ApproverMember(name string) (Owner, bool) {
	want := normalizePerson(name)
	if want == "" {
		return Owner{}, false
	}
	owner, ok := s.ProjectOwner()
	if !ok {
		return Owner{}, false
	}
	if want == normalizePerson(owner.ID) || want == normalizePerson(owner.Name) {
		return owner, true
	}
	return Owner{}, false
}

// normalizePerson folds a name or an id for comparison.
func normalizePerson(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(value)), " ")
}

// SortedMilestones returns the milestones in a stable order, so every derived
// list is deterministic regardless of the order the file was written in.
func (s ProjectSnapshot) SortedMilestones() []Milestone {
	out := make([]Milestone, len(s.Milestones))
	copy(out, s.Milestones)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// OpenMilestones returns the milestones that still need attention.
func (s ProjectSnapshot) OpenMilestones() []Milestone {
	var out []Milestone
	for _, m := range s.SortedMilestones() {
		if m.Status.Open() {
			out = append(out, m)
		}
	}
	return out
}

// Validate checks the structural integrity of a snapshot. It refuses a file
// whose shapes the rules could not reason about: duplicate ids, unknown
// statuses or severities, a progress percentage outside 0..100, a dependency
// on a milestone that does not exist, or a milestone depending on itself.
func (s ProjectSnapshot) Validate() error {
	if strings.TrimSpace(s.ProjectID) == "" {
		return fmt.Errorf("domain: snapshot has no project id")
	}
	if s.Version < 1 {
		return fmt.Errorf("domain: project %q has version %d, want at least 1", s.ProjectID, s.Version)
	}
	if s.UpdatedAt.IsZero() {
		return fmt.Errorf("domain: project %q has no updated_at timestamp", s.ProjectID)
	}

	team := map[string]bool{}
	for _, member := range s.Team {
		if member.IsZero() {
			return fmt.Errorf("domain: project %q has a team member with no id", s.ProjectID)
		}
		if team[member.ID] {
			return fmt.Errorf("domain: project %q lists team member %q twice", s.ProjectID, member.ID)
		}
		team[member.ID] = true
	}

	seen := map[string]bool{}
	for _, m := range s.Milestones {
		if strings.TrimSpace(m.ID) == "" {
			return fmt.Errorf("domain: project %q has a milestone with no id", s.ProjectID)
		}
		if seen[m.ID] {
			return fmt.Errorf("domain: project %q lists milestone %q twice", s.ProjectID, m.ID)
		}
		seen[m.ID] = true
		if !m.Status.Valid() {
			return fmt.Errorf("domain: milestone %q has unknown status %q", m.ID, m.Status)
		}
		if m.ProgressPercent < 0 || m.ProgressPercent > 100 {
			return fmt.Errorf("domain: milestone %q reports %d%% progress, want 0..100", m.ID, m.ProgressPercent)
		}
	}

	for _, m := range s.Milestones {
		for _, dep := range m.DependsOn {
			if dep == m.ID {
				return fmt.Errorf("domain: milestone %q depends on itself", m.ID)
			}
			if !seen[dep] {
				return fmt.Errorf("domain: milestone %q depends on unknown milestone %q", m.ID, dep)
			}
		}
	}

	for _, r := range s.Risks {
		if strings.TrimSpace(r.ID) == "" {
			return fmt.Errorf("domain: project %q has a risk with no id", s.ProjectID)
		}
		if !r.Severity.Valid() {
			return fmt.Errorf("domain: risk %q has unknown severity %q", r.ID, r.Severity)
		}
	}

	return nil
}

// ProjectSummary is what a project list needs, without loading every milestone.
type ProjectSummary struct {
	ProjectID      string    `json:"project_id"`
	Name           string    `json:"name"`
	Version        int64     `json:"version"`
	UpdatedAt      time.Time `json:"updated_at"`
	MilestoneCount int       `json:"milestone_count"`
	OpenCount      int       `json:"open_count"`
}

// Summary derives the list entry of a snapshot.
func (s ProjectSnapshot) Summary() ProjectSummary {
	open := 0
	for _, m := range s.Milestones {
		if m.Status.Open() {
			open++
		}
	}
	return ProjectSummary{
		ProjectID:      s.ProjectID,
		Name:           s.Name,
		Version:        s.Version,
		UpdatedAt:      s.UpdatedAt,
		MilestoneCount: len(s.Milestones),
		OpenCount:      open,
	}
}

// Health is the one-word verdict on the findings.
type Health string

const (
	HealthOnTrack  Health = "on_track"
	HealthAtRisk   Health = "at_risk"
	HealthCritical Health = "critical"
)

// BlockerKind names the rule that produced a blocker. The five kinds are the
// five rules of rules.go; a finding never comes from anywhere else.
type BlockerKind string

const (
	BlockerOverdue             BlockerKind = "overdue"
	BlockerBlockedDependency   BlockerKind = "blocked_dependency"
	BlockerMissingOwner        BlockerKind = "missing_owner"
	BlockerContradictoryStatus BlockerKind = "contradictory_status"
	BlockerStalled             BlockerKind = "stalled"
)

// AllBlockerKinds lists every kind, for tests and documentation.
func AllBlockerKinds() []BlockerKind {
	return []BlockerKind{
		BlockerOverdue,
		BlockerBlockedDependency,
		BlockerMissingOwner,
		BlockerContradictoryStatus,
		BlockerStalled,
	}
}

// Evidence is the source of a finding: which rule fired, on which project and
// milestone, and why (S10).
type Evidence struct {
	Rule        string `json:"rule"`
	ProjectID   string `json:"project_id"`
	MilestoneID string `json:"milestone_id,omitempty"`
	Detail      string `json:"detail"`
}

// Blocker is one problem with one milestone. It always carries a severity, an
// owner state and evidence; a finding that cannot produce all three is not a
// blocker.
type Blocker struct {
	ID            string      `json:"id"`
	Kind          BlockerKind `json:"kind"`
	Severity      Severity    `json:"severity"`
	MilestoneID   string      `json:"milestone_id"`
	MilestoneName string      `json:"milestone_name"`
	Owner         Owner       `json:"owner"`
	OwnerStatus   OwnerStatus `json:"owner_status"`
	Summary       string      `json:"summary"`
	DaysOverdue   int         `json:"days_overdue,omitempty"`
	Evidence      []Evidence  `json:"evidence"`
}

// OverdueItem is a milestone past its due date. It is reported separately from
// the blockers because "late" and "blocked" are different conversations.
type OverdueItem struct {
	MilestoneID   string     `json:"milestone_id"`
	MilestoneName string     `json:"milestone_name"`
	Owner         Owner      `json:"owner"`
	DaysOverdue   int        `json:"days_overdue"`
	Severity      Severity   `json:"severity"`
	Evidence      []Evidence `json:"evidence"`
}

// Findings is everything the deterministic rules derived from one snapshot.
// It is the ground truth the model may explain but never changes.
type Findings struct {
	ProjectID   string        `json:"project_id"`
	Version     int64         `json:"version"`
	GeneratedAt time.Time     `json:"generated_at"`
	Health      Health        `json:"health"`
	Blockers    []Blocker     `json:"blockers"`
	Overdue     []OverdueItem `json:"overdue_items"`
}

// NextAction is one ranked action proposed by the model and admitted by the
// grounding rules. Every action cites at least one derived blocker.
type NextAction struct {
	Rank       int        `json:"rank"`
	Action     string     `json:"action"`
	Owner      Owner      `json:"owner"`
	BlockerIDs []string   `json:"blocker_ids"`
	Rationale  string     `json:"rationale,omitempty"`
	Evidence   []Evidence `json:"evidence"`
}

// Rejection is a model intention the deterministic gate refused, with the
// reason it was refused. Refusals are shown, never silently dropped.
type Rejection struct {
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
	Reason  string `json:"reason"`
}

// PipelineReview is the validated artifact the web app shows: rules-derived
// findings, the model's grounded next actions, the dry-run updates it proposed,
// and every intention the gate refused (S5).
type PipelineReview struct {
	Findings    Findings         `json:"findings"`
	Summary     string           `json:"summary"`
	NextActions []NextAction     `json:"next_actions"`
	Proposals   []ProposedUpdate `json:"proposals"`
	Rejections  []Rejection      `json:"rejections,omitempty"`
}
