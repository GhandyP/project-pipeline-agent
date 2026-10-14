package domain

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Policy is the calendar and SLA policy the rules use. It is configuration, not
// model output: changing it changes the findings, and that is visible.
type Policy struct {
	// OverdueGrace is how long past the due date a milestone may be before it
	// counts as overdue.
	OverdueGrace time.Duration
	// CriticalOverdue is how late a milestone must be to be critical.
	CriticalOverdue time.Duration
	// StalledAfter is how long without an update makes an open milestone
	// stalled.
	StalledAfter time.Duration
	// OverdueHigh is how late a milestone must be to be high severity.
	OverdueHigh time.Duration
}

// DefaultPolicy is the documented default: overdue from the due date on,
// critical after 30 days, high after 7, stalled after 14 days without an update.
func DefaultPolicy() Policy {
	return Policy{
		OverdueGrace:    0,
		OverdueHigh:     7 * 24 * time.Hour,
		CriticalOverdue: 30 * 24 * time.Hour,
		StalledAfter:    14 * 24 * time.Hour,
	}
}

// MaxDays converts a duration to whole days, rounding down.
func days(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	return int(d / (24 * time.Hour))
}

// Derive applies every rule to a snapshot and returns the findings.
//
// This is the only source of blockers, severities and overdue items in the
// system. The model reads this output; it never produces it (S10).
func Derive(snapshot ProjectSnapshot, now time.Time, policy Policy) (Findings, error) {
	if err := snapshot.Validate(); err != nil {
		return Findings{}, err
	}
	now = now.UTC()

	findings := Findings{
		ProjectID:   snapshot.ProjectID,
		Version:     snapshot.Version,
		GeneratedAt: now,
		Blockers:    []Blocker{},
		Overdue:     []OverdueItem{},
	}

	for _, m := range snapshot.SortedMilestones() {
		findings.Blockers = append(findings.Blockers, contradictionsOf(snapshot, m)...)
		if !m.Status.Open() {
			// Closed milestones are history: they are never late, blocked,
			// unowned or stalled.
			continue
		}
		if blocker, item, ok := overdueOf(snapshot, m, now, policy); ok {
			findings.Blockers = append(findings.Blockers, blocker)
			findings.Overdue = append(findings.Overdue, item)
		}
		findings.Blockers = append(findings.Blockers, dependenciesOf(snapshot, m, now, policy)...)
		if blocker, ok := ownerOf(snapshot, m); ok {
			findings.Blockers = append(findings.Blockers, blocker)
		}
		if blocker, ok := stalledOf(snapshot, m, now, policy); ok {
			findings.Blockers = append(findings.Blockers, blocker)
		}
	}

	sortBlockers(findings.Blockers)
	sort.SliceStable(findings.Overdue, func(i, j int) bool {
		if findings.Overdue[i].DaysOverdue != findings.Overdue[j].DaysOverdue {
			return findings.Overdue[i].DaysOverdue > findings.Overdue[j].DaysOverdue
		}
		return findings.Overdue[i].MilestoneID < findings.Overdue[j].MilestoneID
	})
	findings.Health = healthOf(findings.Blockers)
	return findings, nil
}

// blockerID is the stable identity of a finding, so a model can cite it and the
// gate can check the citation.
func blockerID(kind BlockerKind, milestoneID string) string {
	return string(kind) + ":" + milestoneID
}

func newBlocker(snapshot ProjectSnapshot, m Milestone, kind BlockerKind, severity Severity, summary string, evidence Evidence) Blocker {
	evidence.Rule = string(kind)
	evidence.ProjectID = snapshot.ProjectID
	evidence.MilestoneID = m.ID
	return Blocker{
		ID:            blockerID(kind, m.ID),
		Kind:          kind,
		Severity:      severity,
		MilestoneID:   m.ID,
		MilestoneName: m.Name,
		Owner:         m.Owner,
		OwnerStatus:   m.Owner.Status(),
		Summary:       summary,
		Evidence:      []Evidence{evidence},
	}
}

// overdueOf reports a milestone past its due date, and how late it is.
func overdueOf(snapshot ProjectSnapshot, m Milestone, now time.Time, policy Policy) (Blocker, OverdueItem, bool) {
	if m.DueDate.IsZero() {
		// A milestone without a due date cannot be late. The rules say nothing
		// about it instead of inventing a date.
		return Blocker{}, OverdueItem{}, false
	}
	late := now.Sub(m.DueDate.Add(policy.OverdueGrace))
	if late <= 0 {
		return Blocker{}, OverdueItem{}, false
	}
	d := days(now.Sub(m.DueDate))

	severity := SeverityMedium
	switch {
	case now.Sub(m.DueDate) >= policy.CriticalOverdue:
		severity = SeverityCritical
	case now.Sub(m.DueDate) >= policy.OverdueHigh:
		severity = SeverityHigh
	}

	detail := fmt.Sprintf("due %s, %d day(s) late as of %s", m.DueDate.UTC().Format(time.RFC3339), d, now.Format(time.RFC3339))
	blocker := newBlocker(snapshot, m, BlockerOverdue, severity,
		fmt.Sprintf("%q is %d day(s) past its due date", m.Name, d),
		Evidence{Detail: detail})
	blocker.DaysOverdue = d

	item := OverdueItem{
		MilestoneID:   m.ID,
		MilestoneName: m.Name,
		Owner:         m.Owner,
		DaysOverdue:   d,
		Severity:      severity,
		Evidence: []Evidence{{
			Rule:        string(BlockerOverdue),
			ProjectID:   snapshot.ProjectID,
			MilestoneID: m.ID,
			Detail:      detail,
		}},
	}
	return blocker, item, true
}

// dependenciesOf reports a milestone that depends on unfinished work.
func dependenciesOf(snapshot ProjectSnapshot, m Milestone, now time.Time, policy Policy) []Blocker {
	var out []Blocker
	for _, depID := range m.DependsOn {
		dep, ok := snapshot.Milestone(depID)
		if !ok || !dep.Status.Open() {
			continue
		}
		severity := SeverityMedium
		depLate := !dep.DueDate.IsZero() && now.After(dep.DueDate.Add(policy.OverdueGrace))
		if depLate {
			severity = SeverityHigh
		}
		detail := fmt.Sprintf("depends on %q (%s, status %s)", dep.Name, dep.ID, dep.Status)
		out = append(out, newBlocker(snapshot, m, BlockerBlockedDependency, severity,
			fmt.Sprintf("%q waits on %q, which is still %s", m.Name, dep.Name, dep.Status),
			Evidence{Detail: detail}))
	}
	return out
}

// ownerOf reports an open milestone nobody is answerable for.
func ownerOf(snapshot ProjectSnapshot, m Milestone) (Blocker, bool) {
	if !m.Owner.IsZero() {
		return Blocker{}, false
	}
	severity := SeverityLow
	if m.Status != StatusPlanned {
		severity = SeverityMedium
	}
	return newBlocker(snapshot, m, BlockerMissingOwner, severity,
		fmt.Sprintf("%q has no owner while it is %s", m.Name, m.Status),
		Evidence{Detail: "the milestone has no owner id in the tracker"}), true
}

// stalledOf reports an open milestone that has not moved inside the SLA window.
func stalledOf(snapshot ProjectSnapshot, m Milestone, now time.Time, policy Policy) (Blocker, bool) {
	severity := SeverityLow
	if m.Status != StatusPlanned {
		severity = SeverityMedium
	}
	detail := "the milestone has never been updated"
	if !m.LastUpdateAt.IsZero() {
		since := now.Sub(m.LastUpdateAt)
		if since <= policy.StalledAfter {
			return Blocker{}, false
		}
		detail = fmt.Sprintf("last update %s, %d day(s) ago", m.LastUpdateAt.UTC().Format(time.RFC3339), days(since))
	}
	return newBlocker(snapshot, m, BlockerStalled, severity,
		fmt.Sprintf("%q has not been updated inside the %d-day window", m.Name, days(policy.StalledAfter)),
		Evidence{Detail: detail}), true
}

// contradictionsOf reports milestone states that contradict themselves. Unlike
// the other rules it also inspects closed milestones, because "done at 40%" is
// exactly the contradiction a status review must surface.
func contradictionsOf(snapshot ProjectSnapshot, m Milestone) []Blocker {
	var out []Blocker

	if m.Status == StatusDone && m.ProgressPercent < 100 {
		out = append(out, newBlocker(snapshot, m, BlockerContradictoryStatus, SeverityHigh,
			fmt.Sprintf("%q is done but reports %d%% progress", m.Name, m.ProgressPercent),
			Evidence{Detail: fmt.Sprintf("status=done with progress_percent=%d", m.ProgressPercent)}))
	}
	if m.Status.Open() && m.ProgressPercent >= 100 {
		out = append(out, newBlocker(snapshot, m, BlockerContradictoryStatus, SeverityHigh,
			fmt.Sprintf("%q reports %d%% progress but is still %s", m.Name, m.ProgressPercent, m.Status),
			Evidence{Detail: fmt.Sprintf("progress_percent=%d with status=%s", m.ProgressPercent, m.Status)}))
	}
	if m.Status == StatusBlocked && strings.TrimSpace(m.BlockedReason) == "" {
		out = append(out, newBlocker(snapshot, m, BlockerContradictoryStatus, SeverityMedium,
			fmt.Sprintf("%q is blocked without a stated reason", m.Name),
			Evidence{Detail: "status=blocked with an empty blocked_reason"}))
	}
	if m.Status != StatusBlocked && strings.TrimSpace(m.BlockedReason) != "" {
		out = append(out, newBlocker(snapshot, m, BlockerContradictoryStatus, SeverityMedium,
			fmt.Sprintf("%q carries a blocked reason while it is %s", m.Name, m.Status),
			Evidence{Detail: fmt.Sprintf("blocked_reason set with status=%s", m.Status)}))
	}

	return out
}

// sortBlockers orders findings by severity, then kind, then milestone, so two
// runs over the same snapshot always return the same list.
func sortBlockers(blockers []Blocker) {
	sort.SliceStable(blockers, func(i, j int) bool {
		a, b := blockers[i], blockers[j]
		if a.Severity.Rank() != b.Severity.Rank() {
			return a.Severity.Rank() > b.Severity.Rank()
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.MilestoneID < b.MilestoneID
	})
}

// healthOf is the one-word verdict on a blocker list.
func healthOf(blockers []Blocker) Health {
	health := HealthOnTrack
	for _, b := range blockers {
		switch b.Severity {
		case SeverityCritical:
			return HealthCritical
		case SeverityHigh, SeverityMedium:
			health = HealthAtRisk
		}
	}
	return health
}
