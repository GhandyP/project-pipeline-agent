// Package ports declares the boundaries the pipeline crosses: storage, the
// clock and the agent stream. The concrete adapters live in their own packages,
// so the review logic runs in tests with no file, no credential and no network.
package ports

import (
	"context"
	"errors"
	"time"

	"github.com/GhandyP/project-pipeline-agent/internal/domain"
)

// Errors the boundary shares with its callers, so a handler can answer "not
// found" without importing the adapter that produced it.
var (
	// ErrNotFound reports a project, a proposal or an update that does not exist.
	ErrNotFound = errors.New("ports: not found")
	// ErrInvalidID reports an id that is refused before it reaches storage.
	ErrInvalidID = errors.New("ports: invalid id")
	// ErrCommittedWithoutAudit reports the one case the storage contract cannot
	// make atomic: the project was written and the trail entry was not. A caller
	// must never report this as a plain failure, because the change exists.
	ErrCommittedWithoutAudit = errors.New("ports: the project was committed but the audit record was not written")
)

// Clock is the only place the pipeline reads the current time. Injecting it
// keeps the rules, the SLA windows and the audit timestamps testable.
type Clock interface {
	Now() time.Time
}

// ClockFunc adapts a function to Clock.
type ClockFunc func() time.Time

// Now implements Clock.
func (f ClockFunc) Now() time.Time { return f() }

// Tracker is the storage boundary (S7).
//
// Apply is the only method that may mutate a project, and it must perform the
// whole read-check-write in one critical section: version comparison,
// idempotency ledger, project write and audit append either all happen or none
// does. An implementation that cannot promise that is not a Tracker.
type Tracker interface {
	// ListProjects returns every project in the store, ordered by project id.
	ListProjects(ctx context.Context) ([]domain.ProjectSummary, error)
	// Snapshot returns one project at its current version.
	Snapshot(ctx context.Context, projectID string) (domain.ProjectSnapshot, error)
	// SaveProposal stores a dry-run update so a human decision can refer to it
	// by id instead of trusting a client to post the change set back.
	SaveProposal(ctx context.Context, proposal domain.ProposedUpdate) error
	// Proposal loads a stored dry-run update.
	Proposal(ctx context.Context, projectID, updateID string) (domain.ProposedUpdate, error)
	// Apply re-checks the stored proposal against the current project, writes
	// it, bumps the version and appends an audit record. Replaying an already
	// applied update id returns the recorded outcome without writing again.
	Apply(ctx context.Context, projectID, updateID string, approval domain.Approval) (domain.AppliedUpdate, error)
	// Decline records a refused update. It never touches the project.
	Decline(ctx context.Context, projectID, updateID string, approval domain.Approval) (domain.AuditRecord, error)
	// Audit returns the append-only trail of one project, oldest first.
	Audit(ctx context.Context, projectID string) ([]domain.AuditRecord, error)
}

// Review event kinds. A stream emits start, then any number of text, tool_call
// and tool_result events, then exactly one review event, then done.
const (
	EventStart      = "start"
	EventText       = "text"
	EventToolCall   = "tool_call"
	EventToolResult = "tool_result"
	EventReview     = "review"
	EventDone       = "done"
	EventError      = "error"
)

// ReviewEvent is one step of a review run, shaped for Server-Sent Events.
type ReviewEvent struct {
	Kind   string                 `json:"kind"`
	Text   string                 `json:"text,omitempty"`
	Tool   string                 `json:"tool,omitempty"`
	Args   string                 `json:"args,omitempty"`
	Result string                 `json:"result,omitempty"`
	Review *domain.PipelineReview `json:"review,omitempty"`
	Error  string                 `json:"error,omitempty"`
}

// ReviewStreamer runs the ADK review of one project.
//
// emit is called synchronously, in order, and its error stops the run: a client
// that disconnects must be able to stop the agent instead of leaking a run.
// The final review event carries the grounded PipelineReview.
type ReviewStreamer interface {
	Stream(ctx context.Context, projectID string, emit func(ReviewEvent) error) error
}
