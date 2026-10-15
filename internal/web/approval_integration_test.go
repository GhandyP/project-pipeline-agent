package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GhandyP/project-pipeline-agent/internal/domain"
	"github.com/GhandyP/project-pipeline-agent/internal/service"
	"github.com/GhandyP/project-pipeline-agent/internal/tracker"
)

func TestDecisionRejectsNonOwnerWith403AndLeavesStoredStateUntouched(t *testing.T) {
	store, err := tracker.New(t.TempDir(), fixedClock{at: fixedNow})
	if err != nil {
		t.Fatalf("tracker.New: %v", err)
	}
	seeded, err := store.Seed(filepath.Join("..", "..", "testdata", "projects"))
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}
	if seeded != 6 {
		t.Fatalf("seeded %d projects, want 6", seeded)
	}
	svc, err := service.New(store, fixedClock{at: fixedNow}, domain.DefaultPolicy())
	if err != nil {
		t.Fatalf("service.New: %v", err)
	}
	proposal, _, err := svc.Propose(context.Background(), "cobalt-billing", "integration owner-gate test", []domain.ModelChange{
		{MilestoneID: "m-2", Field: "owner_id", Value: "u-sofia"},
	})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}

	projectPath := filepath.Join(store.Dir(), "projects", "cobalt-billing.json")
	proposalPath := filepath.Join(store.Dir(), "proposals", "cobalt-billing", proposal.UpdateID+".json")
	auditPath := filepath.Join(store.Dir(), "audit", "cobalt-billing.jsonl")
	projectBefore := readBytes(t, projectPath)
	proposalBefore := readBytes(t, proposalPath)
	auditBefore := readBytes(t, auditPath)

	server, err := New(Config{Service: svc, Clock: fixedClock{at: fixedNow}})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost,
		"/api/projects/cobalt-billing/updates/"+proposal.UpdateID+"/decision",
		strings.NewReader(`{"approve":true,"by":"Luis Prado"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "project owner") {
		t.Errorf("response = %s, want the project-owner refusal", response.Body.String())
	}
	for _, file := range []struct {
		name string
		path string
		want []byte
	}{
		{name: "project", path: projectPath, want: projectBefore},
		{name: "proposal", path: proposalPath, want: proposalBefore},
		{name: "audit", path: auditPath, want: auditBefore},
	} {
		if got := readBytes(t, file.path); string(got) != string(file.want) {
			t.Errorf("%s changed after the rejected HTTP decision", file.name)
		}
	}
	if snapshot, err := store.Snapshot(context.Background(), "cobalt-billing"); err != nil || snapshot.Version != 11 {
		t.Errorf("project snapshot after rejection = version %d, err %v; want version 11", snapshot.Version, err)
	}
	if records, err := store.Audit(context.Background(), "cobalt-billing"); err != nil || len(records) != 1 || records[0].Action != domain.AuditProposed {
		t.Errorf("audit after rejection = %+v, err %v; want only the proposed record", records, err)
	}
}

func readBytes(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return content
}
