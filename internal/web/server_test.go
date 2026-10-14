package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GhandyP/project-pipeline-agent/internal/domain"
	"github.com/GhandyP/project-pipeline-agent/internal/ports"
	"github.com/GhandyP/project-pipeline-agent/internal/service"
)

var fixedNow = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

type fixedClock struct{ at time.Time }

func (c fixedClock) Now() time.Time { return c.at }

type decisionCall struct {
	projectID string
	updateID  string
	decision  service.Decision
}

// fakeService records every call so a test can prove that a refused request
// never reached the write path.
type fakeService struct {
	projects    []domain.ProjectSummary
	snapshot    domain.ProjectSnapshot
	snapshotErr error
	findings    domain.Findings
	findingsErr error

	proposal    domain.ProposedUpdate
	proposeErr  error
	proposalErr error
	rejections  []domain.Rejection

	applied   domain.AppliedUpdate
	record    domain.AuditRecord
	decideErr error

	audit []domain.AuditRecord

	calls     []string
	decisions []decisionCall
}

func (f *fakeService) ListProjects(context.Context) ([]domain.ProjectSummary, error) {
	f.calls = append(f.calls, "ListProjects")
	return f.projects, nil
}

func (f *fakeService) Snapshot(context.Context, string) (domain.ProjectSnapshot, error) {
	f.calls = append(f.calls, "Snapshot")
	return f.snapshot, f.snapshotErr
}

func (f *fakeService) Findings(context.Context, string) (domain.Findings, error) {
	f.calls = append(f.calls, "Findings")
	return f.findings, f.findingsErr
}

func (f *fakeService) Propose(_ context.Context, projectID, rationale string, changes []domain.ModelChange) (domain.ProposedUpdate, []domain.Rejection, error) {
	f.calls = append(f.calls, "Propose")
	if f.proposeErr != nil {
		return domain.ProposedUpdate{}, f.rejections, f.proposeErr
	}
	return f.proposal, f.rejections, nil
}

func (f *fakeService) StoredProposal(context.Context, string, string) (domain.ProposedUpdate, error) {
	f.calls = append(f.calls, "StoredProposal")
	return f.proposal, f.proposalErr
}

func (f *fakeService) Decide(_ context.Context, projectID, updateID string, decision service.Decision) (domain.AppliedUpdate, domain.AuditRecord, error) {
	f.calls = append(f.calls, "Decide")
	f.decisions = append(f.decisions, decisionCall{projectID: projectID, updateID: updateID, decision: decision})
	if f.decideErr != nil {
		return domain.AppliedUpdate{}, domain.AuditRecord{}, f.decideErr
	}
	if !decision.Approved {
		return domain.AppliedUpdate{}, f.record, nil
	}
	return f.applied, domain.AuditRecord{}, nil
}

func (f *fakeService) Audit(context.Context, string) ([]domain.AuditRecord, error) {
	f.calls = append(f.calls, "Audit")
	return f.audit, nil
}

var _ Service = (*fakeService)(nil)

type fakeStreamer struct {
	events       []ports.ReviewEvent
	err          error
	projectID    string
	streamCalls  int
	firstEmitErr error
}

func (f *fakeStreamer) Stream(_ context.Context, projectID string, emit func(ports.ReviewEvent) error) error {
	f.streamCalls++
	f.projectID = projectID
	for _, event := range f.events {
		if err := emit(event); err != nil {
			f.firstEmitErr = err
			return err
		}
	}
	return f.err
}

func newServer(t *testing.T, svc *fakeService, streamer ports.ReviewStreamer) *Server {
	t.Helper()
	server, err := New(Config{
		Service:  svc,
		Streamer: streamer,
		Policy:   domain.DefaultPolicy(),
		Clock:    fixedClock{at: fixedNow},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return server
}

// defaultService is a service with one project and one blocker.
func defaultService() *fakeService {
	return &fakeService{
		projects: []domain.ProjectSummary{{ProjectID: "cobalt-billing", Name: "Cobalt billing split", Version: 11, MilestoneCount: 2, OpenCount: 2}},
		snapshot: domain.ProjectSnapshot{ProjectID: "cobalt-billing", Name: "Cobalt billing split", Version: 11, UpdatedAt: fixedNow},
		findings: domain.Findings{
			ProjectID: "cobalt-billing",
			Version:   11,
			Health:    domain.HealthAtRisk,
			Blockers: []domain.Blocker{{
				ID:          "overdue:m-1",
				Kind:        domain.BlockerOverdue,
				Severity:    domain.SeverityHigh,
				MilestoneID: "m-1",
				Summary:     `"Invoice numbering service" is late`,
				Evidence:    []domain.Evidence{{Rule: "overdue", ProjectID: "cobalt-billing", MilestoneID: "m-1", Detail: "14 day(s) late"}},
			}},
		},
		proposal: domain.ProposedUpdate{UpdateID: "upd-abc123", ProjectID: "cobalt-billing", ExpectedVersion: 11, DryRun: true, Changes: []domain.Change{{MilestoneID: "m-2", Field: domain.FieldOwnerID, Before: "u-ana", After: "u-sofia"}}},
		applied:  domain.AppliedUpdate{UpdateID: "upd-abc123", ProjectID: "cobalt-billing", ExpectedVersion: 11, ResultingVersion: 12, Approval: domain.Approval{By: "ana", At: fixedNow}},
		record:   domain.AuditRecord{UpdateID: "upd-abc123", ProjectID: "cobalt-billing", Action: domain.AuditDeclined, Approval: domain.Approval{By: "ana", At: fixedNow}},
	}
}

func do(t *testing.T, server *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, path, reader)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	return recorder
}

func decodeBody(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response %q: %v", recorder.Body.String(), err)
	}
	return payload
}

func TestNewRefusesMissingConfiguration(t *testing.T) {
	if _, err := New(Config{Clock: fixedClock{at: fixedNow}}); err == nil {
		t.Error("New accepted a nil service")
	}
	if _, err := New(Config{Service: &fakeService{}}); err == nil {
		t.Error("New accepted a nil clock")
	}
}

func TestIndexAndAssetsAreServed(t *testing.T) {
	server := newServer(t, defaultService(), &fakeStreamer{})

	index := do(t, server, http.MethodGet, "/", "")
	if index.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", index.Code)
	}
	if contentType := index.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/html") {
		t.Errorf("GET / content type = %q, want text/html", contentType)
	}
	if !strings.Contains(index.Body.String(), "No project data changes until a project member approves it") {
		t.Error("the page does not state the approval rule")
	}

	css := do(t, server, http.MethodGet, "/assets/app.css", "")
	if css.Code != http.StatusOK || !strings.HasPrefix(css.Header().Get("Content-Type"), "text/css") {
		t.Errorf("GET /assets/app.css = %d %q, want 200 text/css", css.Code, css.Header().Get("Content-Type"))
	}

	js := do(t, server, http.MethodGet, "/assets/app.js", "")
	if js.Code != http.StatusOK || !strings.HasPrefix(js.Header().Get("Content-Type"), "text/javascript") {
		t.Errorf("GET /assets/app.js = %d %q, want 200 text/javascript", js.Code, js.Header().Get("Content-Type"))
	}

	if got := index.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := index.Header().Get("Content-Security-Policy"); !strings.Contains(got, "default-src 'self'") {
		t.Errorf("Content-Security-Policy = %q, want the self-only policy", got)
	}
}

func TestProjectListCarriesHealthAndCounts(t *testing.T) {
	server := newServer(t, defaultService(), &fakeStreamer{})
	recorder := do(t, server, http.MethodGet, "/api/projects", "")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	payload := decodeBody(t, recorder)
	projects, ok := payload["projects"].([]any)
	if !ok || len(projects) != 1 {
		t.Fatalf("projects = %v, want one entry", payload["projects"])
	}
	entry := projects[0].(map[string]any)
	if entry["health"] != string(domain.HealthAtRisk) {
		t.Errorf("health = %v, want %q", entry["health"], domain.HealthAtRisk)
	}
	if entry["blockers"] != float64(1) {
		t.Errorf("blockers = %v, want 1", entry["blockers"])
	}
}

func TestProjectDetailReturnsTheSnapshotAndTheFindings(t *testing.T) {
	server := newServer(t, defaultService(), &fakeStreamer{})
	recorder := do(t, server, http.MethodGet, "/api/projects/cobalt-billing", "")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	payload := decodeBody(t, recorder)
	snapshot, ok := payload["snapshot"].(map[string]any)
	if !ok || snapshot["project_id"] != "cobalt-billing" {
		t.Fatalf("snapshot = %v", payload["snapshot"])
	}
	findings, ok := payload["findings"].(map[string]any)
	if !ok || findings["health"] != string(domain.HealthAtRisk) {
		t.Fatalf("findings = %v", payload["findings"])
	}
}

// TestInvalidIDsAreRefusedWithoutReachingTheService is the path guard.
func TestInvalidIDsAreRefusedWithoutReachingTheService(t *testing.T) {
	cases := []string{
		"/api/projects/bad%20id",
		"/api/projects/..%2Fetc",
		"/api/projects/.hidden",
		"/api/projects/" + strings.Repeat("x", 65),
		"/api/projects/cobalt-billing/updates/bad%20id",
	}
	for _, path := range cases {
		t.Run(path, func(t *testing.T) {
			service := defaultService()
			server := newServer(t, service, &fakeStreamer{})
			recorder := do(t, server, http.MethodGet, path, "")
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", recorder.Code)
			}
			if len(service.calls) != 0 {
				t.Errorf("the service was called for %q: %v", path, service.calls)
			}
		})
	}
}

func TestProposalIsServed(t *testing.T) {
	server := newServer(t, defaultService(), &fakeStreamer{})
	recorder := do(t, server, http.MethodGet, "/api/projects/cobalt-billing/updates/upd-abc123", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	payload := decodeBody(t, recorder)
	proposal, ok := payload["proposal"].(map[string]any)
	if !ok || proposal["update_id"] != "upd-abc123" || proposal["dry_run"] != true {
		t.Fatalf("proposal = %v", payload["proposal"])
	}
}

func TestAMissingProposalIsNotFound(t *testing.T) {
	service := defaultService()
	service.proposalErr = fmt.Errorf("%w: update", ports.ErrNotFound)
	server := newServer(t, service, &fakeStreamer{})
	recorder := do(t, server, http.MethodGet, "/api/projects/cobalt-billing/updates/upd-missing", "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
}

func TestProposeReturnsTheDryRunAndTheRefusals(t *testing.T) {
	service := defaultService()
	service.rejections = []domain.Rejection{{Kind: "change", Subject: "m-2/budget", Reason: "not an allowlisted field"}}
	server := newServer(t, service, &fakeStreamer{})

	body := `{"rationale":"give the console a second owner","changes":[{"milestone_id":"m-2","field":"owner_id","value":"u-sofia"}]}`
	recorder := do(t, server, http.MethodPost, "/api/projects/cobalt-billing/updates", body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	payload := decodeBody(t, recorder)
	if _, ok := payload["proposal"].(map[string]any); !ok {
		t.Errorf("payload has no proposal: %v", payload)
	}
	rejections, ok := payload["rejections"].([]any)
	if !ok || len(rejections) != 1 {
		t.Errorf("rejections = %v, want one", payload["rejections"])
	}
}

func TestProposeWithNothingAdmissibleIsUnprocessable(t *testing.T) {
	service := defaultService()
	service.proposeErr = domain.ErrNoChanges
	service.rejections = []domain.Rejection{{Kind: "change", Subject: "m-2/budget", Reason: "not an allowlisted field"}}
	server := newServer(t, service, &fakeStreamer{})

	body := `{"rationale":"","changes":[{"milestone_id":"m-2","field":"budget","value":"1000"}]}`
	recorder := do(t, server, http.MethodPost, "/api/projects/cobalt-billing/updates", body)
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", recorder.Code)
	}
	payload := decodeBody(t, recorder)
	if payload["error"] == nil {
		t.Error("the refusal carries no error message")
	}
	if rejections, ok := payload["rejections"].([]any); !ok || len(rejections) != 1 {
		t.Errorf("rejections = %v, want the refusal reasons", payload["rejections"])
	}
}

func TestMalformedAndOversizedBodiesAreRefused(t *testing.T) {
	server := newServer(t, defaultService(), &fakeStreamer{})

	malformed := do(t, server, http.MethodPost, "/api/projects/cobalt-billing/updates", "{not json")
	if malformed.Code != http.StatusBadRequest {
		t.Errorf("malformed body = %d, want 400", malformed.Code)
	}

	twoObjects := do(t, server, http.MethodPost, "/api/projects/cobalt-billing/updates", `{"rationale":"a"}{"rationale":"b"}`)
	if twoObjects.Code != http.StatusBadRequest {
		t.Errorf("two objects = %d, want 400", twoObjects.Code)
	}

	huge := `{"rationale":"` + strings.Repeat("x", maxBodyBytes+16) + `","changes":[]}`
	oversized := do(t, server, http.MethodPost, "/api/projects/cobalt-billing/updates", huge)
	if oversized.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body = %d, want 413", oversized.Code)
	}
}

// TestDecisionRefusesAnAnonymousApproverBeforeTheService is the approval gate at
// the edge: an unnamed decision never reaches a write path.
func TestDecisionRefusesAnAnonymousApproverBeforeTheService(t *testing.T) {
	cases := []string{`{"approve":true,"by":""}`, `{"approve":true,"by":" "}`, `{"approve":true,"by":"a"}`, `{"approve":false,"by":"a"}`}
	for _, body := range cases {
		t.Run(body, func(t *testing.T) {
			service := defaultService()
			server := newServer(t, service, &fakeStreamer{})
			recorder := do(t, server, http.MethodPost, "/api/projects/cobalt-billing/updates/upd-abc123/decision", body)
			if recorder.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422", recorder.Code)
			}
			if len(service.decisions) != 0 {
				t.Errorf("the service was asked to decide: %+v", service.decisions)
			}
		})
	}
}

func TestApprovalAppliesAndDeclineOnlyRecords(t *testing.T) {
	t.Run("approve", func(t *testing.T) {
		service := defaultService()
		server := newServer(t, service, &fakeStreamer{})
		recorder := do(t, server, http.MethodPost, "/api/projects/cobalt-billing/updates/upd-abc123/decision", `{"approve":true,"by":"  ana  ","note":"looks right"}`)

		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
		}
		payload := decodeBody(t, recorder)
		applied, ok := payload["applied"].(map[string]any)
		if !ok || applied["resulting_version"] != float64(12) {
			t.Fatalf("applied = %v", payload["applied"])
		}
		if _, declined := payload["record"]; declined {
			t.Error("an approval also returned a decline record")
		}
		if len(service.decisions) != 1 {
			t.Fatalf("decisions = %+v, want one", service.decisions)
		}
		decision := service.decisions[0]
		if !decision.decision.Approved || decision.decision.By != "ana" || decision.decision.Note != "looks right" {
			t.Errorf("decision = %+v, want the trimmed approval", decision.decision)
		}
		if decision.projectID != "cobalt-billing" || decision.updateID != "upd-abc123" {
			t.Errorf("decision targeted %s/%s", decision.projectID, decision.updateID)
		}
	})

	t.Run("decline", func(t *testing.T) {
		service := defaultService()
		server := newServer(t, service, &fakeStreamer{})
		recorder := do(t, server, http.MethodPost, "/api/projects/cobalt-billing/updates/upd-abc123/decision", `{"approve":false,"by":"luis"}`)

		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
		}
		payload := decodeBody(t, recorder)
		if _, applied := payload["applied"]; applied {
			t.Error("a decline returned an applied update")
		}
		record, ok := payload["record"].(map[string]any)
		if !ok || record["action"] != domain.AuditDeclined {
			t.Fatalf("record = %v, want a declined record", payload["record"])
		}
		if len(service.decisions) != 1 || service.decisions[0].decision.Approved {
			t.Errorf("decisions = %+v, want one refusal", service.decisions)
		}
	})
}

func TestAStaleApplyIsAConflict(t *testing.T) {
	service := defaultService()
	service.decideErr = fmt.Errorf("%w: expected version 11, project is at 12", domain.ErrStaleVersion)
	server := newServer(t, service, &fakeStreamer{})

	recorder := do(t, server, http.MethodPost, "/api/projects/cobalt-billing/updates/upd-abc123/decision", `{"approve":true,"by":"ana"}`)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", recorder.Code, recorder.Body.String())
	}
	payload := decodeBody(t, recorder)
	if message, _ := payload["error"].(string); !strings.Contains(message, "expected version 11") {
		t.Errorf("error = %q, want it to name the versions", message)
	}
}

func TestAuditReturnsAnEmptyArrayNotNull(t *testing.T) {
	server := newServer(t, defaultService(), &fakeStreamer{})
	recorder := do(t, server, http.MethodGet, "/api/projects/cobalt-billing/audit", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if body := strings.TrimSpace(recorder.Body.String()); body != `{"records":[]}` {
		t.Errorf("body = %s, want an empty records array", body)
	}
}

// TestReviewStreamEmitsTheAgentStepsInOrder covers the SSE protocol the UI
// reads.
func TestReviewStreamEmitsTheAgentStepsInOrder(t *testing.T) {
	review := domain.PipelineReview{
		Findings: domain.Findings{ProjectID: "cobalt-billing", Version: 11, Health: domain.HealthAtRisk},
		Summary:  "One milestone is late.",
		Proposals: []domain.ProposedUpdate{
			{UpdateID: "upd-abc123", ProjectID: "cobalt-billing", ExpectedVersion: 11, DryRun: true},
		},
	}
	streamer := &fakeStreamer{events: []ports.ReviewEvent{
		{Kind: ports.EventStart, Text: "Reviewing project cobalt-billing"},
		{Kind: ports.EventToolCall, Tool: "get_project_status", Args: `{"project_id":"cobalt-billing"}`},
		{Kind: ports.EventToolResult, Tool: "get_project_status", Result: `{"findings":{"health":"at_risk"}}`},
		{Kind: ports.EventText, Text: "One milestone is late."},
		{Kind: ports.EventReview, Review: &review},
		{Kind: ports.EventDone, Text: "review complete"},
	}}
	server := newServer(t, defaultService(), streamer)

	recorder := do(t, server, http.MethodPost, "/api/projects/cobalt-billing/review", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if contentType := recorder.Header().Get("Content-Type"); contentType != "text/event-stream" {
		t.Errorf("content type = %q, want text/event-stream", contentType)
	}
	body := recorder.Body.String()
	for _, want := range []string{"event: start", "event: tool_call", "event: tool_result", "event: review", "event: done"} {
		if !strings.Contains(body, want) {
			t.Errorf("the stream does not carry %q:\n%s", want, body)
		}
	}
	reviewAt := strings.Index(body, "event: review")
	doneAt := strings.Index(body, "event: done")
	if reviewAt < 0 || doneAt < 0 || reviewAt > doneAt {
		t.Errorf("event order is wrong: review at %d, done at %d", reviewAt, doneAt)
	}
	if !strings.Contains(body, `"update_id":"upd-abc123"`) {
		t.Error("the review event does not carry the dry-run proposal")
	}
	if streamer.projectID != "cobalt-billing" {
		t.Errorf("streamer reviewed %q, want cobalt-billing", streamer.projectID)
	}
	if streamer.firstEmitErr != nil {
		t.Errorf("emit failed: %v", streamer.firstEmitErr)
	}
}

func TestReviewWithoutAnAgentIsUnavailable(t *testing.T) {
	server := newServer(t, defaultService(), nil)
	recorder := do(t, server, http.MethodPost, "/api/projects/cobalt-billing/review", "")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
}

func TestReviewOfAnUnknownProjectIsNotFound(t *testing.T) {
	service := defaultService()
	service.snapshotErr = fmt.Errorf("%w: project", ports.ErrNotFound)
	streamer := &fakeStreamer{}
	server := newServer(t, service, streamer)

	recorder := do(t, server, http.MethodPost, "/api/projects/nope/review", "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
	if streamer.streamCalls != 0 {
		t.Errorf("the streamer ran %d time(s) for an unknown project", streamer.streamCalls)
	}
}

// TestAStreamFailureStillEndsTheResponse proves a broken run does not hang the
// client and does not report done.
func TestAStreamFailureStillEndsTheResponse(t *testing.T) {
	streamer := &fakeStreamer{
		events: []ports.ReviewEvent{{Kind: ports.EventStart, Text: "Reviewing project cobalt-billing"}},
		err:    errors.New("the model is unavailable"),
	}
	server := newServer(t, defaultService(), streamer)

	recorder := do(t, server, http.MethodPost, "/api/projects/cobalt-billing/review", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 once the stream started", recorder.Code)
	}
	body := recorder.Body.String()
	if strings.Contains(body, "event: done") {
		t.Errorf("a failed run reported done:\n%s", body)
	}
	if !strings.Contains(body, "event: start") {
		t.Errorf("the stream carried no events:\n%s", body)
	}
}

// TestAnApprovalFromOutsideTheProjectIsForbidden is finding 2 at the edge: a
// decision from somebody who is not on the project team cannot approve.
func TestAnApprovalFromOutsideTheProjectIsForbidden(t *testing.T) {
	service := defaultService()
	service.decideErr = fmt.Errorf("%w: %q is not on the team of project %q", domain.ErrNotApprover, "Outsider", "cobalt-billing")
	server := newServer(t, service, &fakeStreamer{})

	recorder := do(t, server, http.MethodPost, "/api/projects/cobalt-billing/updates/upd-abc123/decision", `{"approve":true,"by":"Outsider"}`)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "not on the team") {
		t.Errorf("body = %s, want it to name the reason", recorder.Body.String())
	}
}

// TestAMissingDecisionFieldIsRefused is finding 6: a body that never states the
// decision is a mistake, not a refusal.
func TestAMissingDecisionFieldIsRefused(t *testing.T) {
	service := defaultService()
	server := newServer(t, service, &fakeStreamer{})

	recorder := do(t, server, http.MethodPost, "/api/projects/cobalt-billing/updates/upd-abc123/decision", `{"by":"Ana Ruiz"}`)
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", recorder.Code, recorder.Body.String())
	}
	if len(service.decisions) != 0 {
		t.Errorf("a decision that was never stated reached the service: %+v", service.decisions)
	}

	withDecision := do(t, server, http.MethodPost, "/api/projects/cobalt-billing/updates/upd-abc123/decision", `{"approve":true,"by":"Ana Ruiz"}`)
	if withDecision.Code != http.StatusOK {
		t.Fatalf("status with approve present = %d, want 200: %s", withDecision.Code, withDecision.Body.String())
	}
}

// TestACommittedChangeWithoutItsTrailEntryIsReported is finding 4 at the edge:
// the response must not hide a change that exists.
func TestACommittedChangeWithoutItsTrailEntryIsReported(t *testing.T) {
	service := defaultService()
	service.decideErr = fmt.Errorf("%w: project %q is at version 12 and the trail entry for upd-abc123 is missing: read-only file system",
		ports.ErrCommittedWithoutAudit, "cobalt-billing")
	server := newServer(t, service, &fakeStreamer{})

	recorder := do(t, server, http.MethodPost, "/api/projects/cobalt-billing/updates/upd-abc123/decision", `{"approve":true,"by":"Ana Ruiz"}`)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "is at version 12") {
		t.Errorf("body = %s, want it to say the change exists", recorder.Body.String())
	}
}

func TestAnEmptyBodyOnAReviewIsFine(t *testing.T) {
	server := newServer(t, defaultService(), &fakeStreamer{events: []ports.ReviewEvent{{Kind: ports.EventDone}}})
	request := httptest.NewRequest(http.MethodPost, "/api/projects/cobalt-billing/review", nil)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
}
