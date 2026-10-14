// Package tracker stores projects as JSON files and is the only package that
// may change one.
//
// The store is deliberately boring: one file per project, one file per dry-run
// proposal, one append-only audit trail per project. Every write goes through a
// temporary file and a rename, every mutation happens under a per-project lock,
// and Apply re-checks the stored proposal against the project it is about to
// change. A proposal that has already been applied returns its recorded outcome
// instead of writing again, which is what makes replay harmless (S11, S13).
package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GhandyP/project-pipeline-agent/internal/domain"
	"github.com/GhandyP/project-pipeline-agent/internal/ports"
)

// Errors a caller can tell apart without reading a message.
var (
	// ErrNotFound means the project, proposal or update does not exist.
	ErrNotFound = errors.New("tracker: not found")
	// ErrInvalidID means an id could escape the store directory, so it is
	// refused before it reaches the filesystem.
	ErrInvalidID = errors.New("tracker: invalid id")
	// ErrAlreadyDecided means the update id already has a decision recorded.
	ErrAlreadyDecided = errors.New("tracker: the update was already decided")
)

// idPattern is the only shape of project and update id the store accepts. It
// has no dot and no slash, so a request cannot walk out of the data directory.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Store is a Tracker over a directory of JSON files.
type Store struct {
	dir   string
	clock ports.Clock

	// mu guards locks: the per-project mutex has to exist before it can be used
	// by two callers at once.
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// New opens (and creates) a store rooted at dir.
func New(dir string, clock ports.Clock) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("tracker: a data directory is required")
	}
	if clock == nil {
		return nil, errors.New("tracker: a clock is required")
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("tracker: resolve %q: %w", dir, err)
	}
	for _, sub := range []string{"projects", "proposals", "audit"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			return nil, fmt.Errorf("tracker: create %s: %w", sub, err)
		}
	}
	return &Store{dir: root, clock: clock, locks: map[string]*sync.Mutex{}}, nil
}

// Dir is the resolved store directory, so a caller can report where the data
// lives without guessing.
func (s *Store) Dir() string { return s.dir }

// lock returns the per-project mutex, creating it on first use.
func (s *Store) lock(projectID string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.locks[projectID] == nil {
		s.locks[projectID] = &sync.Mutex{}
	}
	return s.locks[projectID]
}

func validID(id string) bool { return idPattern.MatchString(id) }

func (s *Store) projectPath(projectID string) (string, error) {
	if !validID(projectID) {
		return "", fmt.Errorf("%w: project id %q", ErrInvalidID, projectID)
	}
	return filepath.Join(s.dir, "projects", projectID+".json"), nil
}

func (s *Store) proposalPath(projectID, updateID string) (string, error) {
	if !validID(projectID) {
		return "", fmt.Errorf("%w: project id %q", ErrInvalidID, projectID)
	}
	if !validID(updateID) {
		return "", fmt.Errorf("%w: update id %q", ErrInvalidID, updateID)
	}
	return filepath.Join(s.dir, "proposals", projectID, updateID+".json"), nil
}

func (s *Store) auditPath(projectID string) (string, error) {
	if !validID(projectID) {
		return "", fmt.Errorf("%w: project id %q", ErrInvalidID, projectID)
	}
	return filepath.Join(s.dir, "audit", projectID+".jsonl"), nil
}

// ListProjects returns every project, ordered by project id.
func (s *Store) ListProjects(ctx context.Context) ([]domain.ProjectSummary, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "projects"))
	if err != nil {
		return nil, fmt.Errorf("tracker: list projects: %w", err)
	}
	var out []domain.ProjectSummary
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		projectID := strings.TrimSuffix(entry.Name(), ".json")
		if !validID(projectID) {
			// A stray file the store did not write is ignored instead of
			// being loaded as a project.
			continue
		}
		snapshot, err := s.Snapshot(ctx, projectID)
		if err != nil {
			return nil, err
		}
		out = append(out, snapshot.Summary())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProjectID < out[j].ProjectID })
	return out, nil
}

// Snapshot returns one project, refusing a file that is not a valid snapshot.
func (s *Store) Snapshot(ctx context.Context, projectID string) (domain.ProjectSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return domain.ProjectSnapshot{}, err
	}
	path, err := s.projectPath(projectID)
	if err != nil {
		return domain.ProjectSnapshot{}, err
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return domain.ProjectSnapshot{}, fmt.Errorf("%w: project %q", ErrNotFound, projectID)
	}
	if err != nil {
		return domain.ProjectSnapshot{}, fmt.Errorf("tracker: read project %q: %w", projectID, err)
	}
	var snapshot domain.ProjectSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return domain.ProjectSnapshot{}, fmt.Errorf("tracker: decode project %q: %w", projectID, err)
	}
	if err := snapshot.Validate(); err != nil {
		return domain.ProjectSnapshot{}, err
	}
	if snapshot.ProjectID != projectID {
		return domain.ProjectSnapshot{}, fmt.Errorf("tracker: file %s holds project %q", path, snapshot.ProjectID)
	}
	return snapshot, nil
}

// SaveProposal stores a dry-run update and records it in the audit trail. It
// touches no project.
func (s *Store) SaveProposal(ctx context.Context, proposal domain.ProposedUpdate) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !proposal.DryRun {
		return errors.New("tracker: only a dry-run proposal may be stored")
	}
	path, err := s.proposalPath(proposal.ProjectID, proposal.UpdateID)
	if err != nil {
		return err
	}

	lock := s.lock(proposal.ProjectID)
	lock.Lock()
	defer lock.Unlock()

	if _, err := os.Stat(path); err == nil {
		// The same update id always carries the same change set, so storing it
		// twice is a replay, not a conflict.
		return nil
	}
	if err := writeJSON(path, proposal); err != nil {
		return err
	}
	record := domain.AuditRecord{
		UpdateID:        proposal.UpdateID,
		ProjectID:       proposal.ProjectID,
		Action:          domain.AuditProposed,
		ExpectedVersion: proposal.ExpectedVersion,
		Changes:         proposal.Changes,
		At:              s.clock.Now().UTC(),
	}
	return s.appendAudit(record)
}

// Proposal loads a stored dry-run update.
func (s *Store) Proposal(ctx context.Context, projectID, updateID string) (domain.ProposedUpdate, error) {
	if err := ctx.Err(); err != nil {
		return domain.ProposedUpdate{}, err
	}
	path, err := s.proposalPath(projectID, updateID)
	if err != nil {
		return domain.ProposedUpdate{}, err
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return domain.ProposedUpdate{}, fmt.Errorf("%w: update %q of project %q", ErrNotFound, updateID, projectID)
	}
	if err != nil {
		return domain.ProposedUpdate{}, fmt.Errorf("tracker: read update %q: %w", updateID, err)
	}
	var proposal domain.ProposedUpdate
	if err := json.Unmarshal(raw, &proposal); err != nil {
		return domain.ProposedUpdate{}, fmt.Errorf("tracker: decode update %q: %w", updateID, err)
	}
	if proposal.ProjectID != projectID || proposal.UpdateID != updateID {
		return domain.ProposedUpdate{}, fmt.Errorf("tracker: update file %s does not hold update %q of project %q", path, updateID, projectID)
	}
	return proposal, nil
}

// Apply is the only method that mutates a project.
//
// Everything happens under the project lock: load the proposal, refuse a
// decision that already exists, re-check the proposal against the current
// project, write the new version, then append the audit record. A failure at
// any step leaves the project at its previous version and the trail unchanged.
func (s *Store) Apply(ctx context.Context, projectID, updateID string, approval domain.Approval) (domain.AppliedUpdate, error) {
	if err := ctx.Err(); err != nil {
		return domain.AppliedUpdate{}, err
	}
	approval = approval.Normalized()
	if err := approval.Validate(); err != nil {
		return domain.AppliedUpdate{}, err
	}
	projectPath, err := s.projectPath(projectID)
	if err != nil {
		return domain.AppliedUpdate{}, err
	}
	proposalPath, err := s.proposalPath(projectID, updateID)
	if err != nil {
		return domain.AppliedUpdate{}, err
	}

	lock := s.lock(projectID)
	lock.Lock()
	defer lock.Unlock()

	proposal, err := decodeProposal(proposalPath, projectID, updateID)
	if err != nil {
		return domain.AppliedUpdate{}, err
	}

	records, err := s.readAudit(projectID)
	if err != nil {
		return domain.AppliedUpdate{}, err
	}
	if record, ok := findDecision(records, updateID); ok {
		switch record.Action {
		case domain.AuditApplied:
			// A replay of an approved update: return the recorded outcome and
			// write nothing.
			return appliedFrom(record), nil
		default:
			return domain.AppliedUpdate{}, fmt.Errorf("%w: update %q was %s by %s at %s",
				ErrAlreadyDecided, updateID, record.Action, record.Approval.By, record.At.Format(time.RFC3339))
		}
	}

	snapshot, err := s.Snapshot(ctx, projectID)
	if err != nil {
		return domain.AppliedUpdate{}, err
	}
	if err := proposal.ValidateAgainst(snapshot); err != nil {
		return domain.AppliedUpdate{}, err
	}

	applied, err := snapshot.Apply(proposal.Changes, s.clock.Now().UTC())
	if err != nil {
		return domain.AppliedUpdate{}, err
	}
	if err := writeJSON(projectPath, applied); err != nil {
		return domain.AppliedUpdate{}, err
	}

	at := s.clock.Now().UTC()
	record := domain.AuditRecord{
		UpdateID:         updateID,
		ProjectID:        projectID,
		Action:           domain.AuditApplied,
		ExpectedVersion:  proposal.ExpectedVersion,
		ResultingVersion: applied.Version,
		Changes:          proposal.Changes,
		Approval:         approval,
		At:               at,
	}
	if err := s.appendAudit(record); err != nil {
		return domain.AppliedUpdate{}, err
	}

	return domain.AppliedUpdate{
		UpdateID:         updateID,
		ProjectID:        projectID,
		ExpectedVersion:  proposal.ExpectedVersion,
		ResultingVersion: applied.Version,
		Changes:          proposal.Changes,
		Approval:         approval,
		AppliedAt:        at,
	}, nil
}

// Decline records a refused update and touches no project. Replaying the same
// decline returns the recorded entry; a decline of an already applied update
// fails closed.
func (s *Store) Decline(ctx context.Context, projectID, updateID string, approval domain.Approval) (domain.AuditRecord, error) {
	if err := ctx.Err(); err != nil {
		return domain.AuditRecord{}, err
	}
	approval = approval.Normalized()
	if err := approval.Validate(); err != nil {
		return domain.AuditRecord{}, err
	}
	if _, err := s.proposalPath(projectID, updateID); err != nil {
		return domain.AuditRecord{}, err
	}

	lock := s.lock(projectID)
	lock.Lock()
	defer lock.Unlock()

	proposal, err := s.Proposal(ctx, projectID, updateID)
	if err != nil {
		return domain.AuditRecord{}, err
	}

	records, err := s.readAudit(projectID)
	if err != nil {
		return domain.AuditRecord{}, err
	}
	if record, ok := findDecision(records, updateID); ok {
		if record.Action == domain.AuditDeclined {
			return record, nil
		}
		return domain.AuditRecord{}, fmt.Errorf("%w: update %q was %s by %s at %s",
			ErrAlreadyDecided, updateID, record.Action, record.Approval.By, record.At.Format(time.RFC3339))
	}

	record := domain.AuditRecord{
		UpdateID:        updateID,
		ProjectID:       projectID,
		Action:          domain.AuditDeclined,
		ExpectedVersion: proposal.ExpectedVersion,
		Changes:         proposal.Changes,
		Approval:        approval,
		At:              s.clock.Now().UTC(),
	}
	if err := s.appendAudit(record); err != nil {
		return domain.AuditRecord{}, err
	}
	return record.WithDigest(), nil
}

// Audit returns the project's trail, oldest first, with each record's digest
// recomputed so a reader compares a value, not a claim.
func (s *Store) Audit(ctx context.Context, projectID string) ([]domain.AuditRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	records, err := s.readAudit(projectID)
	if err != nil {
		return nil, err
	}
	for i := range records {
		records[i] = records[i].WithDigest()
	}
	return records, nil
}

// Seed copies the project files of a fixture directory into the store, without
// overwriting a project that is already there. It is how the offline demo and
// the local web app get their starting data.
func (s *Store) Seed(fixtureDir string) (int, error) {
	entries, err := os.ReadDir(fixtureDir)
	if err != nil {
		return 0, fmt.Errorf("tracker: read fixtures %s: %w", fixtureDir, err)
	}
	seeded := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(fixtureDir, entry.Name()))
		if err != nil {
			return seeded, fmt.Errorf("tracker: read fixture %s: %w", entry.Name(), err)
		}
		var snapshot domain.ProjectSnapshot
		if err := json.Unmarshal(raw, &snapshot); err != nil {
			return seeded, fmt.Errorf("tracker: decode fixture %s: %w", entry.Name(), err)
		}
		if err := snapshot.Validate(); err != nil {
			return seeded, fmt.Errorf("tracker: fixture %s: %w", entry.Name(), err)
		}
		path, err := s.projectPath(snapshot.ProjectID)
		if err != nil {
			return seeded, err
		}
		if _, err := os.Stat(path); err == nil {
			continue
		}
		if err := writeJSON(path, snapshot); err != nil {
			return seeded, err
		}
		seeded++
	}
	return seeded, nil
}

// readAudit decodes the trail. A missing trail is an empty trail, not an error.
func (s *Store) readAudit(projectID string) ([]domain.AuditRecord, error) {
	path, err := s.auditPath(projectID)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("tracker: read audit of %q: %w", projectID, err)
	}
	var out []domain.AuditRecord
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var record domain.AuditRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			return nil, fmt.Errorf("tracker: decode audit line of %q: %w", projectID, err)
		}
		out = append(out, record)
	}
	return out, nil
}

// appendAudit adds one line to the trail. It must be called under the project
// lock, because it appends to a file a concurrent reader may be reading.
func (s *Store) appendAudit(record domain.AuditRecord) error {
	path, err := s.auditPath(record.ProjectID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("tracker: create audit directory: %w", err)
	}
	record = record.WithDigest()
	line, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("tracker: encode audit record: %w", err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("tracker: open audit trail: %w", err)
	}
	defer file.Close()
	if _, err := file.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("tracker: append audit record: %w", err)
	}
	return file.Sync()
}

// findDecision returns the recorded decision for one update id.
func findDecision(records []domain.AuditRecord, updateID string) (domain.AuditRecord, bool) {
	for _, record := range records {
		if record.UpdateID != updateID {
			continue
		}
		if record.Action == domain.AuditApplied || record.Action == domain.AuditDeclined {
			return record, true
		}
	}
	return domain.AuditRecord{}, false
}

func appliedFrom(record domain.AuditRecord) domain.AppliedUpdate {
	return domain.AppliedUpdate{
		UpdateID:         record.UpdateID,
		ProjectID:        record.ProjectID,
		ExpectedVersion:  record.ExpectedVersion,
		ResultingVersion: record.ResultingVersion,
		Changes:          record.Changes,
		Approval:         record.Approval,
		AppliedAt:        record.At,
	}
}

func decodeProposal(path, projectID, updateID string) (domain.ProposedUpdate, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return domain.ProposedUpdate{}, fmt.Errorf("%w: update %q of project %q", ErrNotFound, updateID, projectID)
	}
	if err != nil {
		return domain.ProposedUpdate{}, fmt.Errorf("tracker: read update %q: %w", updateID, err)
	}
	var proposal domain.ProposedUpdate
	if err := json.Unmarshal(raw, &proposal); err != nil {
		return domain.ProposedUpdate{}, fmt.Errorf("tracker: decode update %q: %w", updateID, err)
	}
	return proposal, nil
}

// writeJSON writes a file atomically: a temporary file in the same directory,
// then a rename, so a reader never sees a half-written project.
func writeJSON(path string, value any) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("tracker: encode %s: %w", filepath.Base(path), err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("tracker: create %s: %w", filepath.Dir(path), err)
	}
	temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("tracker: create temporary file: %w", err)
	}
	tempName := temp.Name()
	if _, err := temp.Write(append(encoded, '\n')); err != nil {
		temp.Close()
		os.Remove(tempName)
		return fmt.Errorf("tracker: write temporary file: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		os.Remove(tempName)
		return fmt.Errorf("tracker: sync temporary file: %w", err)
	}
	if err := temp.Close(); err != nil {
		os.Remove(tempName)
		return fmt.Errorf("tracker: close temporary file: %w", err)
	}
	if err := os.Rename(tempName, path); err != nil {
		os.Remove(tempName)
		return fmt.Errorf("tracker: replace %s: %w", filepath.Base(path), err)
	}
	return nil
}

// Compile-time proof that the store is the storage boundary the pipeline uses.
var _ ports.Tracker = (*Store)(nil)
