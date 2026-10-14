package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"

	"github.com/GhandyP/project-pipeline-agent/internal/domain"
	"github.com/GhandyP/project-pipeline-agent/internal/ports"
)

var referenceNow = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

// pipelineActions is a real in-memory Actions implementation over a fixture. The
// scripted model is therefore exercised against the real rules and the real
// grounding gate, not against a stub that could drift away from them.
type pipelineActions struct {
	snapshot    domain.ProjectSnapshot
	policy      domain.Policy
	now         time.Time
	snapshotErr error

	proposals   []domain.ProposedUpdate
	groundCalls int
}

func (p *pipelineActions) Snapshot(_ context.Context, projectID string) (domain.ProjectSnapshot, error) {
	if p.snapshotErr != nil {
		return domain.ProjectSnapshot{}, p.snapshotErr
	}
	if p.snapshot.ProjectID != projectID {
		return domain.ProjectSnapshot{}, fmt.Errorf("agent test: no project %q", projectID)
	}
	return p.snapshot, nil
}

func (p *pipelineActions) Findings(ctx context.Context, projectID string) (domain.Findings, error) {
	snapshot, err := p.Snapshot(ctx, projectID)
	if err != nil {
		return domain.Findings{}, err
	}
	return domain.Derive(snapshot, p.now, p.policy)
}

func (p *pipelineActions) Propose(_ context.Context, _ string, rationale string, changes []domain.ModelChange) (domain.ProposedUpdate, []domain.Rejection, error) {
	proposal, rejections, err := domain.ProposeChanges(p.snapshot, p.now, rationale, changes)
	if err != nil {
		return domain.ProposedUpdate{}, rejections, err
	}
	p.proposals = append(p.proposals, proposal)
	return proposal, rejections, nil
}

func (p *pipelineActions) Ground(ctx context.Context, projectID string, review domain.ModelReview) (domain.PipelineReview, error) {
	p.groundCalls++
	snapshot, err := p.Snapshot(ctx, projectID)
	if err != nil {
		return domain.PipelineReview{}, err
	}
	findings, err := domain.Derive(snapshot, p.now, p.policy)
	if err != nil {
		return domain.PipelineReview{}, err
	}
	return domain.Ground(snapshot, findings, review, p.now), nil
}

func loadFixture(t *testing.T, name string) domain.ProjectSnapshot {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "projects", name+".json"))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	var snapshot domain.ProjectSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatalf("decode fixture %s: %v", name, err)
	}
	return snapshot
}

func newActions(t *testing.T, fixture string) *pipelineActions {
	t.Helper()
	return &pipelineActions{
		snapshot: loadFixture(t, fixture),
		policy:   domain.DefaultPolicy(),
		now:      referenceNow,
	}
}

// collect runs one review and returns the events, the review and the error.
func collect(t *testing.T, actions *pipelineActions) ([]ports.ReviewEvent, *domain.PipelineReview, error) {
	t.Helper()
	reviewer, err := NewReviewer(NewScriptedModel(), actions)
	if err != nil {
		t.Fatalf("NewReviewer: %v", err)
	}
	var events []ports.ReviewEvent
	streamErr := reviewer.Stream(context.Background(), actions.snapshot.ProjectID, func(event ports.ReviewEvent) error {
		events = append(events, event)
		return nil
	})
	var review *domain.PipelineReview
	for _, event := range events {
		if event.Kind == ports.EventReview {
			review = event.Review
		}
	}
	return events, review, streamErr
}

func kindsOf(events []ports.ReviewEvent) []string {
	var out []string
	for _, event := range events {
		out = append(out, event.Kind)
	}
	return out
}

func toolsOf(events []ports.ReviewEvent) []string {
	var out []string
	for _, event := range events {
		if event.Kind == ports.EventToolCall {
			out = append(out, event.Tool)
		}
	}
	return out
}

func TestNewRefusesMissingDependencies(t *testing.T) {
	actions := &pipelineActions{}
	if _, err := New(nil, actions); err == nil {
		t.Error("New accepted a nil model")
	}
	if _, err := New(NewScriptedModel(), nil); err == nil {
		t.Error("New accepted a nil actions implementation")
	}
	if _, err := NewReviewer(nil, actions); err == nil {
		t.Error("NewReviewer accepted a nil model")
	}
	if _, err := NewReviewer(NewScriptedModel(), nil); err == nil {
		t.Error("NewReviewer accepted a nil actions implementation")
	}
}

// TestToolSurfaceHasNoWriteTool is the safety contract of this package: three
// tools, none of which asks for confirmation because none of them writes.
func TestToolSurfaceHasNoWriteTool(t *testing.T) {
	configs := toolConfigs()
	if len(configs) != 3 {
		t.Fatalf("registered %d tools, want exactly 3", len(configs))
	}
	for _, name := range []string{ToolGetProjectStatus, ToolProposeUpdate, ToolSubmitReview} {
		config, ok := configs[name]
		if !ok {
			t.Fatalf("tool %q is not registered", name)
		}
		if config.Name != name {
			t.Errorf("tool %q registers as %q", name, config.Name)
		}
		if config.RequireConfirmation {
			t.Errorf("tool %q requires confirmation, which means it can write", name)
		}
		if strings.TrimSpace(config.Description) == "" {
			t.Errorf("tool %q has no description for the model", name)
		}
	}
	if _, ok := configs["apply_update"]; ok {
		t.Error("an apply_update tool is registered")
	}
}

func TestInstructionStatesTheSafetyRule(t *testing.T) {
	for _, want := range []string{
		"never change a project",
		"dry-run",
		"writes nothing",
		"deterministic",
		"blocker id",
		"Never claim that a change was applied",
		ToolGetProjectStatus,
		ToolProposeUpdate,
		ToolSubmitReview,
	} {
		if !strings.Contains(instruction, want) {
			t.Errorf("the instruction does not state %q", want)
		}
	}
}

// TestStreamEmitsTheFixedSequence pins the event protocol the web app reads.
func TestStreamEmitsTheFixedSequence(t *testing.T) {
	actions := newActions(t, "blocked")
	events, review, err := collect(t, actions)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	kinds := kindsOf(events)
	if kinds[0] != ports.EventStart {
		t.Errorf("first event = %q, want %q", kinds[0], ports.EventStart)
	}
	if last := kinds[len(kinds)-1]; last != ports.EventDone {
		t.Errorf("last event = %q, want %q", last, ports.EventDone)
	}
	reviews := 0
	for index, kind := range kinds {
		if kind == ports.EventReview {
			reviews++
			if index != len(kinds)-2 {
				t.Errorf("the review event is at %d of %d, want it just before done", index, len(kinds))
			}
		}
		if kind == ports.EventError {
			t.Errorf("unexpected error event: %s", events[index].Error)
		}
	}
	if reviews != 1 {
		t.Fatalf("emitted %d review events, want exactly 1", reviews)
	}

	if got := toolsOf(events); strings.Join(got, ",") != ToolGetProjectStatus+","+ToolSubmitReview {
		t.Errorf("tool calls = %v, want the status read then the review, and no dry run because no owner gap exists", got)
	}
	if review == nil {
		t.Fatal("the review event carries no review")
	}

	// The findings are the rules', computed from the same fixture.
	wantFindings, err := domain.Derive(actions.snapshot, referenceNow, domain.DefaultPolicy())
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if len(review.Findings.Blockers) != len(wantFindings.Blockers) || review.Findings.Health != wantFindings.Health {
		t.Errorf("findings = %d blockers (%s), want %d (%s)",
			len(review.Findings.Blockers), review.Findings.Health, len(wantFindings.Blockers), wantFindings.Health)
	}
	for i, blocker := range review.Findings.Blockers {
		if blocker.ID != wantFindings.Blockers[i].ID {
			t.Errorf("blocker %d = %q, want %q", i, blocker.ID, wantFindings.Blockers[i].ID)
		}
	}
	if len(review.NextActions) != 2 {
		t.Fatalf("next actions = %+v, want the two scripted ones", review.NextActions)
	}
	if strings.Join(review.NextActions[0].BlockerIDs, ",") != "blocked_dependency:m-2" {
		t.Errorf("first action cites %v, want the highest severity blocker", review.NextActions[0].BlockerIDs)
	}
	for _, action := range review.NextActions {
		if len(action.Evidence) == 0 {
			t.Errorf("action %q carries no evidence", action.Action)
		}
	}
	if len(review.Rejections) != 0 {
		t.Errorf("rejections = %+v, want none for a traceable review", review.Rejections)
	}
	if len(review.Proposals) != 0 {
		t.Errorf("proposals = %+v, want none for a project with no owner gap", review.Proposals)
	}
	if actions.groundCalls != 1 {
		t.Errorf("ground calls = %d, want the single tool call", actions.groundCalls)
	}
}

// TestStreamProposesOneDryRunAndChangesNothing is the write boundary of the
// whole feature: the agent proposes, and the project stays where it was.
func TestStreamProposesOneDryRunAndChangesNothing(t *testing.T) {
	actions := newActions(t, "missing-owner")
	versionBefore := actions.snapshot.Version

	events, review, err := collect(t, actions)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := toolsOf(events); strings.Join(got, ",") != ToolGetProjectStatus+","+ToolProposeUpdate+","+ToolSubmitReview {
		t.Errorf("tool calls = %v, want the proposed dry run", got)
	}
	if len(actions.proposals) != 1 {
		t.Fatalf("stored proposals = %+v, want one", actions.proposals)
	}
	stored := actions.proposals[0]
	if !stored.DryRun || stored.ExpectedVersion != versionBefore {
		t.Errorf("stored proposal = %+v, want a dry run against version %d", stored, versionBefore)
	}
	if len(stored.Changes) != 1 || stored.Changes[0].Field != domain.FieldOwnerID || stored.Changes[0].After != "u-ana" {
		t.Errorf("stored changes = %+v, want the first team member assigned", stored.Changes)
	}
	if review == nil || len(review.Proposals) != 1 {
		t.Fatalf("review = %+v, want one proposal", review)
	}
	if !review.Proposals[0].DryRun || review.Proposals[0].UpdateID != stored.UpdateID {
		t.Errorf("review proposal = %+v, want the same dry run", review.Proposals[0])
	}
	if actions.snapshot.Version != versionBefore {
		t.Errorf("project version = %d, want the unchanged %d", actions.snapshot.Version, versionBefore)
	}
	// Every tool result has to say the run was a dry run.
	for _, event := range events {
		if event.Kind == ports.EventToolResult && event.Tool == ToolProposeUpdate {
			if !strings.Contains(event.Result, `"dry_run":true`) {
				t.Errorf("propose_update result = %s, want dry_run true", event.Result)
			}
			for _, claim := range []string{`"applied":true`, `"written":true`} {
				if strings.Contains(event.Result, claim) {
					t.Errorf("propose_update result claims %s", claim)
				}
			}
		}
	}
}

func TestStreamOnAHealthyProjectSubmitsAnEmptyReview(t *testing.T) {
	actions := newActions(t, "healthy")
	events, review, err := collect(t, actions)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := toolsOf(events); strings.Join(got, ",") != ToolGetProjectStatus+","+ToolSubmitReview {
		t.Errorf("tool calls = %v, want no dry run for a project without findings", got)
	}
	if review == nil {
		t.Fatal("the review event carries no review")
	}
	if len(review.Findings.Blockers) != 0 {
		t.Errorf("blockers = %+v, want none", review.Findings.Blockers)
	}
	if len(review.NextActions) != 0 {
		t.Errorf("next actions = %+v, want none", review.NextActions)
	}
	if !strings.Contains(review.Summary, "0 blocker(s) derived for atlas-checkout at version 7") {
		t.Errorf("summary = %q, want the scripted summary to say nothing was found", review.Summary)
	}
	if len(actions.proposals) != 0 {
		t.Errorf("proposals = %+v, want none", actions.proposals)
	}
}

func TestStreamStopsWhenEmitFails(t *testing.T) {
	actions := newActions(t, "blocked")
	reviewer, err := NewReviewer(NewScriptedModel(), actions)
	if err != nil {
		t.Fatalf("NewReviewer: %v", err)
	}

	stop := errors.New("client went away")
	calls := 0
	err = reviewer.Stream(context.Background(), actions.snapshot.ProjectID, func(ports.ReviewEvent) error {
		calls++
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatalf("Stream error = %v, want the emit error", err)
	}
	if calls != 1 {
		t.Errorf("emit calls = %d, want the run to stop at the first failure", calls)
	}
	if actions.groundCalls != 0 {
		t.Errorf("ground calls = %d, want the run never to start", actions.groundCalls)
	}
}

// TestStreamReportsAFailureInsteadOfReviewingNothing makes a broken read fail
// closed and visibly.
func TestStreamReportsAFailureInsteadOfReviewingNothing(t *testing.T) {
	actions := newActions(t, "blocked")
	actions.snapshotErr = errors.New("agent test: the tracker is unreachable")

	events, review, err := collect(t, actions)
	if err == nil {
		t.Fatal("Stream returned no error for an unreachable project")
	}
	if review != nil {
		t.Errorf("a review was emitted despite the failure: %+v", review)
	}
	last := events[len(events)-1]
	if last.Kind != ports.EventError || !strings.Contains(last.Error, "unreachable") {
		t.Errorf("last event = %+v, want an error event naming the failure", last)
	}
	for _, event := range events {
		if event.Kind == ports.EventDone {
			t.Error("the stream reported done after a failure")
		}
	}
}

func TestStreamRefusesAnEmptyProjectID(t *testing.T) {
	reviewer, err := NewReviewer(NewScriptedModel(), &pipelineActions{})
	if err != nil {
		t.Fatalf("NewReviewer: %v", err)
	}
	if err := reviewer.Stream(context.Background(), "   ", func(ports.ReviewEvent) error { return nil }); err == nil {
		t.Fatal("Stream accepted an empty project id")
	}
	if err := reviewer.Stream(context.Background(), "p", nil); err == nil {
		t.Fatal("Stream accepted a nil emit function")
	}
}

// TestScriptedModelIsDeterministic is what makes the offline demo trustworthy:
// the same project produces the same review every time.
func TestScriptedModelIsDeterministic(t *testing.T) {
	firstEvents, firstReview, err := collect(t, newActions(t, "contradictory"))
	if err != nil {
		t.Fatalf("first Stream: %v", err)
	}
	secondEvents, secondReview, err := collect(t, newActions(t, "contradictory"))
	if err != nil {
		t.Fatalf("second Stream: %v", err)
	}

	if strings.Join(kindsOf(firstEvents), ",") != strings.Join(kindsOf(secondEvents), ",") {
		t.Errorf("event kinds differ between runs: %v vs %v", kindsOf(firstEvents), kindsOf(secondEvents))
	}
	if strings.Join(toolsOf(firstEvents), ",") != strings.Join(toolsOf(secondEvents), ",") {
		t.Errorf("tool calls differ between runs: %v vs %v", toolsOf(firstEvents), toolsOf(secondEvents))
	}
	firstJSON, err := json.Marshal(firstReview)
	if err != nil {
		t.Fatalf("marshal first review: %v", err)
	}
	secondJSON, err := json.Marshal(secondReview)
	if err != nil {
		t.Fatalf("marshal second review: %v", err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Errorf("reviews differ between runs:\n%s\n%s", firstJSON, secondJSON)
	}
}

func TestStreamRefusesAnOverlongToolLoop(t *testing.T) {
	// A model that keeps asking for the status without ever submitting a
	// review must not run forever.
	reviewer, err := NewReviewer(loopingModel{}, newActions(t, "blocked"))
	if err != nil {
		t.Fatalf("NewReviewer: %v", err)
	}
	var events []ports.ReviewEvent
	err = reviewer.Stream(context.Background(), "cobalt-billing", func(event ports.ReviewEvent) error {
		events = append(events, event)
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "tool calls without finishing") {
		t.Fatalf("Stream error = %v, want the bounded-loop refusal", err)
	}
	if last := events[len(events)-1]; last.Kind != ports.EventError {
		t.Errorf("last event = %+v, want an error event", last)
	}
}

// loopingModel always asks for the project status again.
type loopingModel struct{}

func (loopingModel) Name() string { return "looping" }

func (loopingModel) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		response, _ := callResponse(ToolGetProjectStatus, GetStatusArgs{ProjectID: "cobalt-billing"})
		yield(response, nil)
	}
}
