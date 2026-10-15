// Package web serves the review web app: a JSON API, a Server-Sent Events
// stream of the agent's review, and the embedded user interface.
//
// The interface is the place where the human decision happens. The API can
// propose a dry run and it can record a decision, but it cannot approve one on
// its own: a decision carries the configured owner's name or id, and the tracker
// re-checks that roster-backed authority before it writes anything (S9, S13).
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/GhandyP/project-pipeline-agent/internal/domain"
	"github.com/GhandyP/project-pipeline-agent/internal/ports"
	"github.com/GhandyP/project-pipeline-agent/internal/service"
)

//go:embed assets
var embedded embed.FS

// maxBodyBytes bounds every request body. A proposal is a handful of changes,
// so anything larger is a mistake or an attack.
const maxBodyBytes = 64 << 10

// idPattern is the only shape of project and update id the API accepts. It
// matches the tracker's own rule, so an id that reaches the store is one the
// store will take.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Service is the use cases the web app needs. It is the service layer's public
// surface, declared here so this package can be tested without the tracker, the
// clock or the agent.
type Service interface {
	ListProjects(ctx context.Context) ([]domain.ProjectSummary, error)
	Snapshot(ctx context.Context, projectID string) (domain.ProjectSnapshot, error)
	Findings(ctx context.Context, projectID string) (domain.Findings, error)
	Propose(ctx context.Context, projectID, rationale string, changes []domain.ModelChange) (domain.ProposedUpdate, []domain.Rejection, error)
	StoredProposal(ctx context.Context, projectID, updateID string) (domain.ProposedUpdate, error)
	Decide(ctx context.Context, projectID, updateID string, decision service.Decision) (domain.AppliedUpdate, domain.AuditRecord, error)
	Audit(ctx context.Context, projectID string) ([]domain.AuditRecord, error)
}

// Config is what the server needs. Streamer may be nil: the read paths and the
// decision path keep working, and asking for a review answers 503 instead of
// pretending a review happened.
type Config struct {
	Service  Service
	Streamer ports.ReviewStreamer
	Policy   domain.Policy
	Clock    ports.Clock
	Logger   *slog.Logger
	// RequestTimeout bounds one API handler. It defaults to 30 seconds and is
	// not applied to the review stream, which is long work by nature.
	RequestTimeout time.Duration
}

// Server holds the wired handlers.
type Server struct {
	service        Service
	streamer       ports.ReviewStreamer
	policy         domain.Policy
	clock          ports.Clock
	logger         *slog.Logger
	requestTimeout time.Duration
	assetFS        fs.FS
	index          []byte
}

// New builds the server, refusing a configuration that could serve nothing.
func New(cfg Config) (*Server, error) {
	if cfg.Service == nil {
		return nil, errors.New("web: a service is required")
	}
	if cfg.Clock == nil {
		return nil, errors.New("web: a clock is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	timeout := cfg.RequestTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	assets, err := fs.Sub(embedded, "assets")
	if err != nil {
		return nil, fmt.Errorf("web: read the embedded assets: %w", err)
	}
	index, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		return nil, fmt.Errorf("web: read index.html: %w", err)
	}
	return &Server{
		service:        cfg.Service,
		streamer:       cfg.Streamer,
		policy:         cfg.Policy,
		clock:          cfg.Clock,
		logger:         logger,
		requestTimeout: timeout,
		assetFS:        assets,
		index:          index,
	}, nil
}

// Handler returns the routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServerFS(s.assetFS)))
	mux.HandleFunc("GET /api/projects", s.handleProjects)
	mux.HandleFunc("GET /api/projects/{id}", s.handleProject)
	mux.HandleFunc("POST /api/projects/{id}/review", s.handleReview)
	mux.HandleFunc("POST /api/projects/{id}/updates", s.handlePropose)
	mux.HandleFunc("GET /api/projects/{id}/updates/{updateID}", s.handleProposal)
	mux.HandleFunc("POST /api/projects/{id}/updates/{updateID}/decision", s.handleDecision)
	mux.HandleFunc("GET /api/projects/{id}/audit", s.handleAudit)
	return s.withHeaders(mux)
}

// withHeaders applies the response policy once, so no handler can forget it.
func (s *Server) withHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'")
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// pages and assets
// ---------------------------------------------------------------------------

func (s *Server) handleIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(s.index)
}

// ---------------------------------------------------------------------------
// reads
// ---------------------------------------------------------------------------

// projectEntry is one row of the project list: what the project is, how it is
// doing, and how much needs attention.
type projectEntry struct {
	Summary  domain.ProjectSummary `json:"summary"`
	Health   domain.Health         `json:"health"`
	Blockers int                   `json:"blockers"`
	Overdue  int                   `json:"overdue"`
}

func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.apiContext(r)
	defer cancel()

	summaries, err := s.service.ListProjects(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	entries := make([]projectEntry, 0, len(summaries))
	for _, summary := range summaries {
		findings, err := s.service.Findings(ctx, summary.ProjectID)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		entries = append(entries, projectEntry{
			Summary:  summary,
			Health:   findings.Health,
			Blockers: len(findings.Blockers),
			Overdue:  len(findings.Overdue),
		})
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"projects": entries})
}

func (s *Server) handleProject(w http.ResponseWriter, r *http.Request) {
	projectID, ok := s.projectID(w, r)
	if !ok {
		return
	}
	ctx, cancel := s.apiContext(r)
	defer cancel()

	snapshot, err := s.service.Snapshot(ctx, projectID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	findings, err := s.service.Findings(ctx, projectID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"snapshot": snapshot, "findings": findings})
}

func (s *Server) handleProposal(w http.ResponseWriter, r *http.Request) {
	projectID, updateID, ok := s.ids(w, r)
	if !ok {
		return
	}
	ctx, cancel := s.apiContext(r)
	defer cancel()

	proposal, err := s.service.StoredProposal(ctx, projectID, updateID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"proposal": proposal})
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	projectID, ok := s.projectID(w, r)
	if !ok {
		return
	}
	ctx, cancel := s.apiContext(r)
	defer cancel()

	records, err := s.service.Audit(ctx, projectID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if records == nil {
		records = []domain.AuditRecord{}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"records": records})
}

// ---------------------------------------------------------------------------
// the review stream
// ---------------------------------------------------------------------------

// handleReview streams one agent review as Server-Sent Events.
//
// The stream is not wrapped in the API timeout: a review is the long work. It
// is still bounded by the request context, so a client that closes the tab
// stops the run.
func (s *Server) handleReview(w http.ResponseWriter, r *http.Request) {
	projectID, ok := s.projectID(w, r)
	if !ok {
		return
	}
	if s.streamer == nil {
		s.writeError(w, http.StatusServiceUnavailable, "the review agent is not configured on this server", nil)
		return
	}
	if _, err := s.service.Snapshot(r.Context(), projectID); err != nil {
		s.fail(w, r, err)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeError(w, http.StatusInternalServerError, "this server cannot stream", nil)
		return
	}
	controller := http.NewResponseController(w)

	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	emit := func(event ports.ReviewEvent) error {
		encoded, err := json.Marshal(event)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Kind, encoded); err != nil {
			return err
		}
		return controller.Flush()
	}

	if err := s.streamer.Stream(r.Context(), projectID, emit); err != nil {
		// The stream already carried an error event; the status line is long
		// sent. Record it so an operator can see it, and stop.
		s.logger.WarnContext(r.Context(), "review stream failed", "project_id", projectID, "error", err)
	}
}

// ---------------------------------------------------------------------------
// proposals and decisions
// ---------------------------------------------------------------------------

type proposeRequest struct {
	Rationale string               `json:"rationale"`
	Changes   []domain.ModelChange `json:"changes"`
}

func (s *Server) handlePropose(w http.ResponseWriter, r *http.Request) {
	projectID, ok := s.projectID(w, r)
	if !ok {
		return
	}
	var body proposeRequest
	if !s.decode(w, r, &body) {
		return
	}
	ctx, cancel := s.apiContext(r)
	defer cancel()

	proposal, rejections, err := s.service.Propose(ctx, projectID, body.Rationale, body.Changes)
	if err != nil {
		if errors.Is(err, domain.ErrNoChanges) {
			s.writeError(w, http.StatusUnprocessableEntity, "no proposed change was admissible", rejections)
			return
		}
		s.fail(w, r, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"proposal": proposal, "rejections": rejections})
}

type decisionRequest struct {
	// Approve is a pointer so a body that never states the decision is refused
	// instead of silently becoming a refusal.
	Approve *bool  `json:"approve"`
	By      string `json:"by"`
	Note    string `json:"note"`
}

func (s *Server) handleDecision(w http.ResponseWriter, r *http.Request) {
	projectID, updateID, ok := s.ids(w, r)
	if !ok {
		return
	}
	var body decisionRequest
	if !s.decode(w, r, &body) {
		return
	}
	if body.Approve == nil {
		s.writeError(w, http.StatusUnprocessableEntity, "the decision must state approve true or false", nil)
		return
	}
	// The approval gate is checked here as well as inside the service, so an
	// anonymous decision never reaches the write path at all.
	approval := domain.Approval{By: body.By, At: s.clock.Now().UTC(), Note: body.Note}.Normalized()
	if err := approval.Validate(); err != nil {
		s.writeError(w, http.StatusUnprocessableEntity, err.Error(), nil)
		return
	}

	ctx, cancel := s.apiContext(r)
	defer cancel()

	applied, record, err := s.service.Decide(ctx, projectID, updateID, service.Decision{
		Approved: *body.Approve,
		By:       approval.By,
		Note:     approval.Note,
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if *body.Approve {
		s.writeJSON(w, http.StatusOK, map[string]any{"applied": applied})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"record": record})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func (s *Server) apiContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), s.requestTimeout)
}

func (s *Server) projectID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !idPattern.MatchString(id) {
		s.writeError(w, http.StatusNotFound, "no such project", nil)
		return "", false
	}
	return id, true
}

func (s *Server) ids(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	projectID, ok := s.projectID(w, r)
	if !ok {
		return "", "", false
	}
	updateID := r.PathValue("updateID")
	if !idPattern.MatchString(updateID) {
		s.writeError(w, http.StatusNotFound, "no such update", nil)
		return "", "", false
	}
	return projectID, updateID, true
}

// decode reads a bounded JSON body, answering 400 or 413 instead of panicking.
func (s *Server) decode(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		status := http.StatusBadRequest
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		message := "the request body is not valid JSON"
		if status == http.StatusRequestEntityTooLarge {
			message = "the request body is larger than 64 KiB"
		}
		s.writeError(w, status, message, nil)
		return false
	}
	// A second value in the body is a mistake worth reporting rather than
	// silently ignoring.
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		s.writeError(w, http.StatusBadRequest, "the request body must hold exactly one JSON object", nil)
		return false
	}
	return true
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(payload); err != nil {
		s.logger.Error("write response", "error", err)
	}
}

func (s *Server) writeError(w http.ResponseWriter, status int, message string, rejections []domain.Rejection) {
	payload := map[string]any{"error": message}
	if len(rejections) > 0 {
		payload["rejections"] = rejections
	}
	s.writeJSON(w, status, payload)
}

// fail maps a domain or store error to the status a client can act on, and logs
// only what the client is not told.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, context.Canceled):
		// The client went away. Nothing to answer.
		s.logger.DebugContext(r.Context(), "request cancelled", "path", r.URL.Path)
		return
	case errors.Is(err, context.DeadlineExceeded):
		s.writeError(w, http.StatusGatewayTimeout, "the request took too long", nil)
	case errors.Is(err, ports.ErrNotFound):
		s.writeError(w, http.StatusNotFound, "no such project or update", nil)
	case errors.Is(err, ports.ErrInvalidID):
		s.writeError(w, http.StatusNotFound, "no such project or update", nil)
	case errors.Is(err, domain.ErrNotApprover):
		s.writeError(w, http.StatusForbidden, "only the configured project owner may approve this update", nil)
	case errors.Is(err, ports.ErrCommittedWithoutAudit):
		// The change exists. Answering with a generic failure would hide a
		// committed write, so the client is told exactly what happened.
		s.writeError(w, http.StatusInternalServerError, err.Error(), nil)
	case errors.Is(err, domain.ErrStaleVersion):
		s.writeError(w, http.StatusConflict, err.Error(), nil)
	case errors.Is(err, domain.ErrDriftedChange):
		s.writeError(w, http.StatusConflict, err.Error(), nil)
	case errors.Is(err, domain.ErrAlreadyApplied):
		s.writeError(w, http.StatusConflict, err.Error(), nil)
	default:
		if isValidationError(err) {
			s.writeError(w, http.StatusUnprocessableEntity, err.Error(), nil)
			return
		}
		s.logger.ErrorContext(r.Context(), "request failed", "path", r.URL.Path, "error", err)
		s.writeError(w, http.StatusInternalServerError, "the request could not be completed", nil)
	}
}

// isValidationError recognises the refusals the domain produces for a bad
// intention, which are the client's fault and not a server failure.
func isValidationError(err error) bool {
	message := err.Error()
	for _, marker := range []string{
		"domain: an approval needs",
		"not one of the allowlisted fields",
		"is not on the project team",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}
