package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/GhandyP/project-pipeline-agent/internal/domain"
	"github.com/GhandyP/project-pipeline-agent/internal/ports"
)

// appName is the ADK application name the runner registers the sessions under.
const appName = "project_pipeline_agent"

// userID is the session owner. The web app is single-user by design: the human
// who approves is named on the approval, not in a login.
const userID = "pipeline-web"

// maxToolCalls bounds one review run, so a model that never converges ends the
// stream with an error instead of spending without a limit.
const maxToolCalls = 24

// Reviewer runs the ADK review of one project and streams what happens.
type Reviewer struct {
	llm     model.LLM
	actions Actions
}

// NewReviewer builds the streaming review runner.
func NewReviewer(llm model.LLM, actions Actions) (*Reviewer, error) {
	if llm == nil {
		return nil, errNoModel
	}
	if actions == nil {
		return nil, errNoActions
	}
	return &Reviewer{llm: llm, actions: actions}, nil
}

// Compile-time proof that the reviewer is the streamer the web and the CLI use.
var _ ports.ReviewStreamer = (*Reviewer)(nil)

// Stream reviews one project and calls emit for every step.
//
// The event order is fixed: one start, then the text, tool_call and tool_result
// events of the run, then exactly one review event carrying the grounded
// PipelineReview, then done. If the model never submitted a review, the rules'
// findings are emitted anyway, so a failed model answer still produces a review
// a human can read. An error from emit stops the run.
func (r *Reviewer) Stream(ctx context.Context, projectID string, emit func(ports.ReviewEvent) error) error {
	if emit == nil {
		return errors.New("agent: an emit function is required")
	}
	if strings.TrimSpace(projectID) == "" {
		return errors.New("agent: a project id is required")
	}

	var mu sync.Mutex
	var grounded *domain.PipelineReview
	collect := func(review domain.PipelineReview) {
		mu.Lock()
		defer mu.Unlock()
		copied := review
		grounded = &copied
	}

	adkAgent, err := newAgent(r.llm, r.actions, collect)
	if err != nil {
		return err
	}
	run, err := runner.New(runner.Config{
		AppName:           appName,
		Agent:             adkAgent,
		SessionService:    session.InMemoryService(),
		AutoCreateSession: true,
	})
	if err != nil {
		return fmt.Errorf("agent: build the runner: %w", err)
	}

	if err := emit(ports.ReviewEvent{Kind: ports.EventStart, Text: "Reviewing project " + projectID}); err != nil {
		return err
	}

	prompt := fmt.Sprintf("Review project %s. Start by calling %s.", projectID, ToolGetProjectStatus)
	toolCalls := 0
	for event, runErr := range run.Run(ctx, userID, projectID, genai.NewContentFromText(prompt, genai.RoleUser), adkagent.RunConfig{}) {
		if runErr != nil {
			return r.fail(emit, fmt.Errorf("agent: review run: %w", runErr))
		}
		if event == nil || event.Content == nil {
			continue
		}
		if event.Author == string(genai.RoleUser) {
			// The runner echoes the prompt as a user event. It is not something
			// the agent did, so it is not streamed as agent output.
			continue
		}
		for _, part := range event.Content.Parts {
			if part == nil {
				continue
			}
			switch {
			case part.FunctionCall != nil:
				toolCalls++
				if err := emit(ports.ReviewEvent{
					Kind: ports.EventToolCall,
					Tool: part.FunctionCall.Name,
					Args: encodePayload(part.FunctionCall.Args),
				}); err != nil {
					return err
				}
			case part.FunctionResponse != nil:
				if err := emit(ports.ReviewEvent{
					Kind:   ports.EventToolResult,
					Tool:   part.FunctionResponse.Name,
					Result: encodePayload(part.FunctionResponse.Response),
				}); err != nil {
					return err
				}
			case strings.TrimSpace(part.Text) != "" && !event.Partial:
				if err := emit(ports.ReviewEvent{Kind: ports.EventText, Text: part.Text}); err != nil {
					return err
				}
			}
		}
		if toolCalls > maxToolCalls {
			return r.fail(emit, fmt.Errorf("agent: the review used more than %d tool calls without finishing", maxToolCalls))
		}
		if err := ctx.Err(); err != nil {
			return r.fail(emit, err)
		}
	}

	if grounded == nil {
		// The model never submitted a review. The rules still have something to
		// say, and a human asked for a review, so the findings are emitted.
		review, err := r.actions.Ground(ctx, projectID, domain.ModelReview{})
		if err != nil {
			return r.fail(emit, err)
		}
		grounded = &review
	}

	if err := emit(ports.ReviewEvent{Kind: ports.EventReview, Review: grounded}); err != nil {
		return err
	}
	return emit(ports.ReviewEvent{Kind: ports.EventDone, Text: "review complete"})
}

// fail reports the failure to the client before returning it, so the stream
// never ends silently.
func (r *Reviewer) fail(emit func(ports.ReviewEvent) error, err error) error {
	if emitErr := emit(ports.ReviewEvent{Kind: ports.EventError, Error: err.Error()}); emitErr != nil {
		return errors.Join(err, emitErr)
	}
	return err
}

// encodePayload renders a tool payload as JSON text for the stream. A payload
// that cannot be encoded is reported as such instead of being dropped.
func encodePayload(payload any) string {
	if payload == nil {
		return ""
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Sprintf("{\"unencodable\":%q}", err.Error())
	}
	return string(encoded)
}
