// Package service holds the use cases a request can reach: read a project,
// derive its findings, run the model's answer through the grounding gate,
// propose a dry-run update, and decide one.
//
// It contains no HTTP and no ADK. Every use case is a method over the ports, so
// the whole pipeline is exercised in tests with an in-memory store and a fixed
// clock, and the model never gets a path to a write that does not pass a named
// approval (S9, S13).
package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/GhandyP/project-pipeline-agent/internal/domain"
	"github.com/GhandyP/project-pipeline-agent/internal/ports"
)

// Service is the pipeline use cases over a tracker, a clock and a policy.
type Service struct {
	store  ports.Tracker
	clock  ports.Clock
	policy domain.Policy
}

// New builds the service. A nil store or clock is refused instead of failing
// later, inside a handler.
func New(store ports.Tracker, clock ports.Clock, policy domain.Policy) (*Service, error) {
	if store == nil {
		return nil, errors.New("service: a tracker is required")
	}
	if clock == nil {
		return nil, errors.New("service: a clock is required")
	}
	return &Service{store: store, clock: clock, policy: policy}, nil
}

// Policy is the calendar and SLA policy this service derives findings with.
func (s *Service) Policy() domain.Policy { return s.policy }

// ListProjects returns the project list.
func (s *Service) ListProjects(ctx context.Context) ([]domain.ProjectSummary, error) {
	return s.store.ListProjects(ctx)
}

// Snapshot returns one project at its current version.
func (s *Service) Snapshot(ctx context.Context, projectID string) (domain.ProjectSnapshot, error) {
	return s.store.Snapshot(ctx, projectID)
}

// Findings derives the rules' view of a project. It reads the clock once, so
// every rule inside one call agrees on "now".
func (s *Service) Findings(ctx context.Context, projectID string) (domain.Findings, error) {
	snapshot, err := s.store.Snapshot(ctx, projectID)
	if err != nil {
		return domain.Findings{}, err
	}
	return domain.Derive(snapshot, s.clock.Now().UTC(), s.policy)
}

// Ground runs the model's answer through the deterministic gate and returns the
// review a human reads.
func (s *Service) Ground(ctx context.Context, projectID string, model domain.ModelReview) (domain.PipelineReview, error) {
	snapshot, err := s.store.Snapshot(ctx, projectID)
	if err != nil {
		return domain.PipelineReview{}, err
	}
	now := s.clock.Now().UTC()
	findings, err := domain.Derive(snapshot, now, s.policy)
	if err != nil {
		return domain.PipelineReview{}, err
	}
	return domain.Ground(snapshot, findings, model, now), nil
}

// Propose validates a change intention set against the current project and
// stores the dry-run update, so a later decision can refer to it by id.
//
// It writes no project: storing a proposal records an intention, never a
// change.
func (s *Service) Propose(ctx context.Context, projectID, rationale string, changes []domain.ModelChange) (domain.ProposedUpdate, []domain.Rejection, error) {
	snapshot, err := s.store.Snapshot(ctx, projectID)
	if err != nil {
		return domain.ProposedUpdate{}, nil, err
	}
	proposal, rejections, err := domain.ProposeChanges(snapshot, s.clock.Now().UTC(), rationale, changes)
	if err != nil {
		return domain.ProposedUpdate{}, rejections, err
	}
	if err := s.store.SaveProposal(ctx, proposal); err != nil {
		return domain.ProposedUpdate{}, rejections, err
	}
	return proposal, rejections, nil
}

// Decision is what a named human decided about a stored update.
type Decision struct {
	Approved bool
	By       string
	Note     string
}

// Decide records a human decision.
//
// An approval goes to the tracker, which re-checks the project before writing;
// a refusal only appends to the trail. The approval is built here, with the
// service's clock, so the recorded moment is the pipeline's moment and not the
// caller's.
func (s *Service) Decide(ctx context.Context, projectID, updateID string, decision Decision) (domain.AppliedUpdate, domain.AuditRecord, error) {
	approval := domain.Approval{
		By:   decision.By,
		At:   s.clock.Now().UTC(),
		Note: decision.Note,
	}.Normalized()
	if err := approval.Validate(); err != nil {
		return domain.AppliedUpdate{}, domain.AuditRecord{}, err
	}

	if !decision.Approved {
		record, err := s.store.Decline(ctx, projectID, updateID, approval)
		if err != nil {
			return domain.AppliedUpdate{}, domain.AuditRecord{}, err
		}
		return domain.AppliedUpdate{}, record, nil
	}

	applied, err := s.store.Apply(ctx, projectID, updateID, approval)
	if err != nil {
		return domain.AppliedUpdate{}, domain.AuditRecord{}, err
	}
	return applied, domain.AuditRecord{}, nil
}

// Audit returns the project's trail.
func (s *Service) Audit(ctx context.Context, projectID string) ([]domain.AuditRecord, error) {
	return s.store.Audit(ctx, projectID)
}

// StoredProposal loads a dry-run update so a client can show what it is about to
// decide on, instead of trusting a change set posted back by the browser.
func (s *Service) StoredProposal(ctx context.Context, projectID, updateID string) (domain.ProposedUpdate, error) {
	return s.store.Proposal(ctx, projectID, updateID)
}

// ErrNoChanges is re-exported so a handler can answer 422 without importing the
// domain, and so the readable check stays in one place.
var ErrNoChanges = domain.ErrNoChanges

// NotFoundError reports a project or update that does not exist.
type NotFoundError struct{ What string }

func (e NotFoundError) Error() string { return fmt.Sprintf("service: %s was not found", e.What) }
