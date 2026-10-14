package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Sentinel errors the service and the tests rely on. A caller can tell "the
// project moved" from "this change was never valid" without reading a message.
var (
	// ErrNoChanges means the intent carried nothing to apply.
	ErrNoChanges = errors.New("domain: the update proposes no change")
	// ErrStaleVersion means the project changed after this update was proposed.
	ErrStaleVersion = errors.New("domain: the project changed after this update was proposed")
	// ErrDriftedChange means a proposed change no longer matches the project.
	ErrDriftedChange = errors.New("domain: the proposed change no longer matches the project")
	// ErrAlreadyApplied means the idempotency ledger already recorded this id.
	ErrAlreadyApplied = errors.New("domain: this update was already recorded")
)

// ChangeField is a tracker field a review may propose changing. The list is a
// deliberately small allowlist: anything else is refused, not passed through.
type ChangeField string

const (
	FieldOwnerID         ChangeField = "owner_id"
	FieldStatus          ChangeField = "status"
	FieldDueDate         ChangeField = "due_date"
	FieldProgressPercent ChangeField = "progress_percent"
	FieldBlockedReason   ChangeField = "blocked_reason"
)

// changeFields is the canonical order in which a change set is validated and
// applied. Status precedes blocked_reason so a single update can block a
// milestone and state why.
var changeFields = []ChangeField{
	FieldOwnerID,
	FieldStatus,
	FieldDueDate,
	FieldProgressPercent,
	FieldBlockedReason,
}

// AllChangeFields lists the allowlisted fields in application order.
func AllChangeFields() []ChangeField {
	out := make([]ChangeField, len(changeFields))
	copy(out, changeFields)
	return out
}

// Valid reports whether the field is allowlisted.
func (f ChangeField) Valid() bool {
	for _, known := range changeFields {
		if known == f {
			return true
		}
	}
	return false
}

func (f ChangeField) order() int {
	for i, known := range changeFields {
		if known == f {
			return i
		}
	}
	return len(changeFields)
}

// fieldValue renders one field of a milestone in its canonical string form, so
// before/after pairs are comparable across the API, the audit trail and a test.
func fieldValue(m Milestone, field ChangeField) string {
	switch field {
	case FieldOwnerID:
		return m.Owner.ID
	case FieldStatus:
		return string(m.Status)
	case FieldDueDate:
		if m.DueDate.IsZero() {
			return ""
		}
		return m.DueDate.UTC().Format(time.RFC3339)
	case FieldProgressPercent:
		return strconv.Itoa(m.ProgressPercent)
	case FieldBlockedReason:
		return m.BlockedReason
	default:
		return ""
	}
}

// Change is one field-level modification of one milestone, with the value it
// replaces. The before value is what makes a stale update detectable.
type Change struct {
	MilestoneID string      `json:"milestone_id"`
	Field       ChangeField `json:"field"`
	Before      string      `json:"before"`
	After       string      `json:"after"`
}

// ProposedUpdate is a dry-run change set: what would happen, against which
// tracker version, under which idempotency key. Building one writes nothing
// (S9, S11).
type ProposedUpdate struct {
	UpdateID        string    `json:"update_id"`
	ProjectID       string    `json:"project_id"`
	ExpectedVersion int64     `json:"expected_version"`
	CreatedAt       time.Time `json:"created_at"`
	Rationale       string    `json:"rationale,omitempty"`
	Changes         []Change  `json:"changes"`
	DryRun          bool      `json:"dry_run"`
}

// Approval is a named human decision. It is the only thing that authorizes a
// tracker mutation, and it is recorded with the action (S13).
type Approval struct {
	By   string    `json:"by"`
	At   time.Time `json:"at"`
	Note string    `json:"note,omitempty"`
}

// Normalized trims the approver name so two spellings of the same name compare
// and store identically.
func (a Approval) Normalized() Approval {
	a.By = strings.TrimSpace(a.By)
	a.Note = strings.TrimSpace(a.Note)
	return a
}

// Validate refuses an anonymous or undated approval. Two characters is the
// minimum that can identify a decision to a reader of the audit trail.
func (a Approval) Validate() error {
	a = a.Normalized()
	if len([]rune(a.By)) < 2 {
		return fmt.Errorf("domain: an approval needs a named approver, got %q", a.By)
	}
	if a.At.IsZero() {
		return errors.New("domain: an approval needs the moment it was made")
	}
	return nil
}

// AppliedUpdate is the outcome of an approved mutation.
type AppliedUpdate struct {
	UpdateID         string    `json:"update_id"`
	ProjectID        string    `json:"project_id"`
	ExpectedVersion  int64     `json:"expected_version"`
	ResultingVersion int64     `json:"resulting_version"`
	Changes          []Change  `json:"changes"`
	Approval         Approval  `json:"approval"`
	AppliedAt        time.Time `json:"applied_at"`
}

// Audit actions. Every proposal, approval and refusal ends up here.
const (
	AuditProposed = "proposed"
	AuditApplied  = "applied"
	AuditDeclined = "declined"
)

// AuditRecord is one append-only entry of the trail. Digest covers the record
// without itself, so a reader can tell an edited record from an original.
type AuditRecord struct {
	UpdateID         string    `json:"update_id"`
	ProjectID        string    `json:"project_id"`
	Action           string    `json:"action"`
	ExpectedVersion  int64     `json:"expected_version"`
	ResultingVersion int64     `json:"resulting_version,omitempty"`
	Changes          []Change  `json:"changes,omitempty"`
	Approval         Approval  `json:"approval,omitempty"`
	At               time.Time `json:"at"`
	Digest           string    `json:"digest,omitempty"`
}

// WithDigest returns the record with its digest filled in.
func (r AuditRecord) WithDigest() AuditRecord {
	r.Digest = ""
	payload, err := json.Marshal(r)
	if err != nil {
		return r
	}
	sum := sha256.Sum256(payload)
	r.Digest = "sha256:" + hex.EncodeToString(sum[:])
	return r
}

// parseDate accepts the two shapes a tracker or a model plausibly sends: a full
// RFC3339 timestamp and a plain calendar date.
func parseDate(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, errors.New("the due date is empty")
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed.UTC(), nil
	}
	if parsed, err := time.Parse("2006-01-02", value); err == nil {
		return parsed.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("due date %q is neither RFC3339 nor YYYY-MM-DD", value)
}

// ProposeChanges validates a model's change intentions against a snapshot and
// builds the dry-run update.
//
// It never returns a partially valid proposal: every intention is either in
// Changes or in the rejections, and a request with no admissible change fails
// with ErrNoChanges. Proposals carry the snapshot version they were built
// against and a deterministic update id, which is what makes replay harmless
// (S11).
func ProposeChanges(snapshot ProjectSnapshot, now time.Time, rationale string, raw []ModelChange) (ProposedUpdate, []Rejection, error) {
	if err := snapshot.Validate(); err != nil {
		return ProposedUpdate{}, nil, err
	}
	now = now.UTC()

	// Validate per milestone, in canonical field order, against a simulated
	// milestone, so a status change and the reason that belongs to it can
	// travel together.
	byMilestone := map[string][]ModelChange{}
	var order []string
	for _, change := range raw {
		id := strings.TrimSpace(change.MilestoneID)
		if _, seen := byMilestone[id]; !seen {
			order = append(order, id)
		}
		byMilestone[id] = append(byMilestone[id], change)
	}
	sort.Strings(order)

	var changes []Change
	var rejections []Rejection
	seenField := map[string]bool{}

	for _, milestoneID := range order {
		milestone, ok := snapshot.Milestone(milestoneID)
		if !ok {
			for _, change := range byMilestone[milestoneID] {
				rejections = append(rejections, Rejection{
					Kind:    "change",
					Subject: subjectOf(change),
					Reason:  fmt.Sprintf("unknown milestone %q", milestoneID),
				})
			}
			continue
		}
		if !milestone.Status.Open() {
			for _, change := range byMilestone[milestoneID] {
				rejections = append(rejections, Rejection{
					Kind:    "change",
					Subject: subjectOf(change),
					Reason:  fmt.Sprintf("milestone %q is %s: closed milestones do not accept updates", milestoneID, milestone.Status),
				})
			}
			continue
		}

		batch := byMilestone[milestoneID]
		sort.SliceStable(batch, func(i, j int) bool { return changeFieldOrder(batch[i].Field) < changeFieldOrder(batch[j].Field) })

		simulated := milestone
		for _, change := range batch {
			key := milestoneID + "/" + string(change.Field)
			if seenField[key] {
				rejections = append(rejections, Rejection{
					Kind:    "change",
					Subject: subjectOf(change),
					Reason:  "the same field is proposed twice for one milestone",
				})
				continue
			}
			applied, err := validateChange(snapshot, simulated, now, change)
			if err != nil {
				rejections = append(rejections, Rejection{Kind: "change", Subject: subjectOf(change), Reason: err.Error()})
				continue
			}
			seenField[key] = true
			changes = append(changes, applied)
			if err := applyChange(&simulated, applied); err != nil {
				return ProposedUpdate{}, rejections, err
			}
		}
	}

	if len(changes) == 0 {
		return ProposedUpdate{}, rejections, ErrNoChanges
	}

	proposal := ProposedUpdate{
		ProjectID:       snapshot.ProjectID,
		ExpectedVersion: snapshot.Version,
		CreatedAt:       now,
		Rationale:       strings.TrimSpace(rationale),
		Changes:         changes,
		DryRun:          true,
	}
	proposal.UpdateID = updateID(proposal)
	return proposal, rejections, nil
}

// validateChange admits one intention and renders it as a Change with its
// before value filled in from the simulated milestone.
func validateChange(snapshot ProjectSnapshot, simulated Milestone, now time.Time, raw ModelChange) (Change, error) {
	field := ChangeField(strings.TrimSpace(string(raw.Field)))
	if !field.Valid() {
		return Change{}, fmt.Errorf("field %q is not one of the allowlisted fields", raw.Field)
	}
	before := fieldValue(simulated, field)
	after := strings.TrimSpace(raw.Value)

	switch field {
	case FieldOwnerID:
		if after == "" {
			return Change{}, errors.New("owner_id is empty; unassigning is not an update this review proposes")
		}
		if _, ok := snapshot.TeamMember(after); !ok {
			return Change{}, fmt.Errorf("owner_id %q is not on the project team, so it cannot be assigned", after)
		}

	case FieldStatus:
		status := MilestoneStatus(after)
		if !status.Valid() {
			return Change{}, fmt.Errorf("status %q is not one of planned, in_progress, blocked, done, cancelled", after)
		}
		if status == StatusDone && simulated.ProgressPercent != 100 {
			return Change{}, fmt.Errorf("status done would contradict progress_percent=%d", simulated.ProgressPercent)
		}
		if status != StatusDone && simulated.ProgressPercent >= 100 {
			return Change{}, fmt.Errorf("status %s would contradict progress_percent=%d", status, simulated.ProgressPercent)
		}

	case FieldDueDate:
		due, err := parseDate(after)
		if err != nil {
			return Change{}, err
		}
		if !due.After(snapshot.UpdatedAt) {
			return Change{}, fmt.Errorf("due date %s is not after the project's last update %s", due.Format(time.RFC3339), snapshot.UpdatedAt.UTC().Format(time.RFC3339))
		}

	case FieldProgressPercent:
		percent, err := strconv.Atoi(after)
		if err != nil {
			return Change{}, fmt.Errorf("progress_percent %q is not a number", after)
		}
		if percent < 0 || percent > 100 {
			return Change{}, fmt.Errorf("progress_percent %d is outside 0..100", percent)
		}
		if (percent == 100) != (simulated.Status == StatusDone) {
			return Change{}, fmt.Errorf("progress_percent %d contradicts status %s", percent, simulated.Status)
		}

	case FieldBlockedReason:
		if after == "" {
			return Change{}, errors.New("blocked_reason is empty; a blank reason is not a change")
		}
		if simulated.Status != StatusBlocked {
			return Change{}, fmt.Errorf("a blocked reason only belongs on a blocked milestone, which is %s", simulated.Status)
		}
	}

	if before == after {
		return Change{}, fmt.Errorf("%s already holds %q", field, after)
	}
	if field == FieldDueDate {
		// Store dates in the canonical shape the rules compare with.
		due, err := parseDate(after)
		if err != nil {
			return Change{}, err
		}
		after = due.Format(time.RFC3339)
	}
	return Change{MilestoneID: simulated.ID, Field: field, Before: before, After: after}, nil
}

// applyChange applies one validated change to a simulated milestone.
func applyChange(m *Milestone, change Change) error {
	switch change.Field {
	case FieldOwnerID:
		m.Owner = Owner{ID: change.After}
	case FieldStatus:
		m.Status = MilestoneStatus(change.After)
	case FieldDueDate:
		due, err := parseDate(change.After)
		if err != nil {
			return err
		}
		m.DueDate = due
	case FieldProgressPercent:
		percent, err := strconv.Atoi(change.After)
		if err != nil {
			return err
		}
		m.ProgressPercent = percent
	case FieldBlockedReason:
		m.BlockedReason = change.After
	default:
		return fmt.Errorf("domain: cannot apply unknown field %q", change.Field)
	}
	return nil
}

// Apply applies a validated change set to a snapshot, returning the new
// version. It assumes ValidateAgainst already ran.
func (s ProjectSnapshot) Apply(changes []Change, now time.Time) (ProjectSnapshot, error) {
	out := s
	out.Milestones = append([]Milestone(nil), s.Milestones...)
	for _, change := range changes {
		for i := range out.Milestones {
			if out.Milestones[i].ID != change.MilestoneID {
				continue
			}
			if err := applyChange(&out.Milestones[i], change); err != nil {
				return ProjectSnapshot{}, err
			}
			if change.Field == FieldOwnerID {
				// validateChange admitted the owner only from the team roster, so
				// the name comes from the roster too.
				if member, ok := s.TeamMember(change.After); ok {
					out.Milestones[i].Owner = member
				}
			}
		}
	}
	out.Version = s.Version + 1
	out.UpdatedAt = now.UTC()
	return out, nil
}

// ValidateAgainst re-checks a stored proposal against the current snapshot at
// the moment of application. It is the last gate before a write: a stale
// version and a drifted field both fail closed (S11, S13).
func (p ProposedUpdate) ValidateAgainst(snapshot ProjectSnapshot) error {
	if p.ProjectID != snapshot.ProjectID {
		return fmt.Errorf("domain: update %s belongs to project %q, not %q", p.UpdateID, p.ProjectID, snapshot.ProjectID)
	}
	if p.ExpectedVersion != snapshot.Version {
		return fmt.Errorf("%w: expected version %d, project is at %d", ErrStaleVersion, p.ExpectedVersion, snapshot.Version)
	}
	if len(p.Changes) == 0 {
		return ErrNoChanges
	}

	simulated := map[string]Milestone{}
	for _, change := range p.Changes {
		current, ok := simulated[change.MilestoneID]
		if !ok {
			milestone, exists := snapshot.Milestone(change.MilestoneID)
			if !exists {
				return fmt.Errorf("%w: milestone %q no longer exists", ErrDriftedChange, change.MilestoneID)
			}
			current = milestone
		}
		if got := fieldValue(current, change.Field); got != change.Before {
			return fmt.Errorf("%w: %s/%s is %q, the update expected %q",
				ErrDriftedChange, change.MilestoneID, change.Field, got, change.Before)
		}
		if _, err := validateChange(snapshot, current, p.CreatedAt, ModelChange{
			MilestoneID: change.MilestoneID,
			Field:       string(change.Field),
			Value:       change.After,
		}); err != nil {
			return fmt.Errorf("%w: %s/%s no longer validates: %v", ErrDriftedChange, change.MilestoneID, change.Field, err)
		}
		if err := applyChange(&current, change); err != nil {
			return err
		}
		simulated[change.MilestoneID] = current
	}
	return nil
}

// updateID derives the idempotency key from the mutation itself, not from a
// timestamp or a random value: proposing the same change against the same
// version twice produces the same id, and applying it twice is then harmless.
func updateID(p ProposedUpdate) string {
	payload := struct {
		ProjectID string   `json:"project_id"`
		Version   int64    `json:"expected_version"`
		Changes   []Change `json:"changes"`
	}{ProjectID: p.ProjectID, Version: p.ExpectedVersion, Changes: p.Changes}
	encoded, err := json.Marshal(payload)
	if err != nil {
		// Changes are plain strings; a failure here is impossible, and an empty
		// suffix still fails closed because the id would not match a replay.
		encoded = []byte(p.ProjectID)
	}
	sum := sha256.Sum256(encoded)
	return "upd-" + hex.EncodeToString(sum[:])[:16]
}

// changeFieldOrder ranks a raw field name in the canonical application order
// without requiring the caller to have parsed it yet.
func changeFieldOrder(raw string) int {
	return ChangeField(strings.TrimSpace(raw)).order()
}

func subjectOf(change ModelChange) string {
	id := strings.TrimSpace(change.MilestoneID)
	field := strings.TrimSpace(string(change.Field))
	if id == "" {
		return field
	}
	return id + "/" + field
}
