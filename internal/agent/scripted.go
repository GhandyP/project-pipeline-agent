package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"regexp"
	"strings"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/GhandyP/project-pipeline-agent/internal/domain"
)

// ScriptedModel is a deterministic language model that drives the full ADK loop
// with no credential and no network.
//
// It exists for two reasons. The offline demo and the whole test suite can then
// exercise the real agent, the real tools and the real runner, and a reviewer
// can read exactly what the model would do. It answers from the conversation
// history alone, so two runs over the same project produce the same result.
type ScriptedModel struct{}

// NewScriptedModel builds the deterministic model.
func NewScriptedModel() *ScriptedModel { return &ScriptedModel{} }

// Name implements model.LLM.
func (m *ScriptedModel) Name() string { return "scripted-deterministic" }

// GenerateContent implements model.LLM. It reads the conversation so far and
// returns the next step: read the status, propose the one update it can justify,
// submit the review, then summarize.
func (m *ScriptedModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		response, err := m.next(req)
		if err != nil {
			yield(nil, err)
			return
		}
		yield(response, nil)
	}
}

// projectIDPattern finds the project id in the prompt the reviewer sends.
var projectIDPattern = regexp.MustCompile(`project ([A-Za-z0-9_-]{1,64})`)

// conversation is everything the scripted model needs from the history.
type conversation struct {
	projectID string
	status    map[string]any
	called    map[string]bool
}

func (m *ScriptedModel) next(req *model.LLMRequest) (*model.LLMResponse, error) {
	if req == nil {
		return textResponse("There is nothing to review."), nil
	}
	seen := scanConversation(req)

	switch {
	case seen.projectID == "":
		return textResponse("I need a project id before I can review anything."), nil
	case !seen.called[ToolGetProjectStatus]:
		return callResponse(ToolGetProjectStatus, GetStatusArgs{ProjectID: seen.projectID})
	case seen.status == nil:
		return textResponse("I could not read the project status, so there is nothing to report."), nil
	}

	var findings domain.Findings
	if err := decodeField(seen.status, "findings", &findings); err != nil {
		// The status tool answered with an error payload instead of findings,
		// so there is nothing to review. Saying that is better than failing the
		// run: the review still ends with the rules' view of the project.
		return textResponse("I could not read the project status, so I am not reviewing it."), nil
	}
	var snapshot domain.ProjectSnapshot
	if err := decodeField(seen.status, "snapshot", &snapshot); err != nil {
		return textResponse("I could not read the project status, so I am not reviewing it."), nil
	}

	summary, actions, changes := scriptedReview(findings, snapshot)

	if len(changes) > 0 && !seen.called[ToolProposeUpdate] {
		return callResponse(ToolProposeUpdate, ProposeUpdateArgs{
			ProjectID: seen.projectID,
			Rationale: summary,
			Changes:   changes,
		})
	}
	if !seen.called[ToolSubmitReview] {
		return callResponse(ToolSubmitReview, SubmitReviewArgs{
			ProjectID:   seen.projectID,
			Summary:     summary,
			NextActions: actions,
			Changes:     changes,
		})
	}
	return textResponse(fmt.Sprintf(
		"The rules derived %d blocker(s) for %s at version %d, with %d overdue item(s). The review is ready for a human decision; nothing was changed.",
		len(findings.Blockers), findings.ProjectID, findings.Version, len(findings.Overdue))), nil
}

// scriptedReview builds the answer a careful model would submit: one ranked
// action per blocker (at most three) and, when a milestone has no owner, the one
// assignment the team roster makes possible.
func scriptedReview(findings domain.Findings, snapshot domain.ProjectSnapshot) (string, []domain.ModelAction, []domain.ModelChange) {
	summary := fmt.Sprintf(
		"%d blocker(s) derived for %s at version %d: %d critical or high, %d overdue item(s).",
		len(findings.Blockers), findings.ProjectID, findings.Version, countSevere(findings.Blockers), len(findings.Overdue))

	var actions []domain.ModelAction
	var changes []domain.ModelChange
	for index, blocker := range findings.Blockers {
		if index >= 3 {
			break
		}
		action := domain.ModelAction{
			Action:     scriptedAction(blocker),
			BlockerIDs: []string{blocker.ID},
			Rationale:  blocker.Summary,
		}
		if !blocker.Owner.IsZero() {
			action.OwnerID = blocker.Owner.ID
		}
		actions = append(actions, action)

		if blocker.Kind == domain.BlockerMissingOwner && len(changes) == 0 && len(snapshot.Team) > 0 {
			changes = append(changes, domain.ModelChange{
				MilestoneID: blocker.MilestoneID,
				Field:       string(domain.FieldOwnerID),
				Value:       snapshot.Team[0].ID,
			})
		}
	}
	return summary, actions, changes
}

func scriptedAction(blocker domain.Blocker) string {
	switch blocker.Kind {
	case domain.BlockerOverdue:
		return fmt.Sprintf("Re-plan %q: it is %d day(s) past its due date", blocker.MilestoneName, blocker.DaysOverdue)
	case domain.BlockerBlockedDependency:
		return fmt.Sprintf("Unblock %q: it waits on unfinished work", blocker.MilestoneName)
	case domain.BlockerMissingOwner:
		return fmt.Sprintf("Assign an owner to %q", blocker.MilestoneName)
	case domain.BlockerContradictoryStatus:
		return fmt.Sprintf("Correct the contradictory status of %q", blocker.MilestoneName)
	case domain.BlockerStalled:
		return fmt.Sprintf("Chase an update on %q", blocker.MilestoneName)
	default:
		return fmt.Sprintf("Review %q", blocker.MilestoneName)
	}
}

func countSevere(blockers []domain.Blocker) int {
	severe := 0
	for _, blocker := range blockers {
		if blocker.Severity.Rank() >= domain.SeverityHigh.Rank() {
			severe++
		}
	}
	return severe
}

// scanConversation reads the history: which tools already ran, what the status
// tool returned, and which project is being reviewed.
func scanConversation(req *model.LLMRequest) conversation {
	seen := conversation{called: map[string]bool{}}
	for _, content := range req.Contents {
		if content == nil {
			continue
		}
		for _, part := range content.Parts {
			if part == nil {
				continue
			}
			if seen.projectID == "" && strings.TrimSpace(part.Text) != "" {
				if match := projectIDPattern.FindStringSubmatch(part.Text); match != nil {
					seen.projectID = match[1]
				}
			}
			if part.FunctionCall != nil {
				seen.called[part.FunctionCall.Name] = true
				if seen.projectID == "" {
					if id, ok := part.FunctionCall.Args["project_id"].(string); ok {
						seen.projectID = id
					}
				}
			}
			if part.FunctionResponse != nil && part.FunctionResponse.Name == ToolGetProjectStatus {
				if response := part.FunctionResponse.Response; response != nil {
					seen.status = response
				}
			}
		}
	}
	return seen
}

// decodeField reads a key of a tool payload into a Go value. The framework
// nests a tool result under its own key in some versions, so both shapes are
// accepted instead of guessing one.
func decodeField(payload map[string]any, key string, target any) error {
	value, ok := payload[key]
	if !ok {
		for _, nested := range []string{"result", "output"} {
			if inner, ok := payload[nested].(map[string]any); ok {
				if value, ok = inner[key]; ok {
					break
				}
			}
		}
	}
	if !ok {
		return fmt.Errorf("payload has no %q field", key)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, target)
}

func textResponse(text string) *model.LLMResponse {
	return &model.LLMResponse{
		Content: genai.NewContentFromText(text, genai.RoleModel),
	}
}

func callResponse(name string, args any) (*model.LLMResponse, error) {
	encoded, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("agent: encode the arguments of %s: %w", name, err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return nil, fmt.Errorf("agent: decode the arguments of %s: %w", name, err)
	}
	return &model.LLMResponse{
		Content: &genai.Content{
			Role: genai.RoleModel,
			Parts: []*genai.Part{{
				FunctionCall: &genai.FunctionCall{Name: name, Args: fields},
			}},
		},
	}, nil
}
