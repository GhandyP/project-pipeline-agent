// Package agent builds the ADK agent that reviews a project pipeline and the
// three tools it may call.
//
// The agent explains and prioritizes. It does not decide what is true: the
// findings come from the rules in internal/domain, and the agent has no tool
// that can change a project. Its review is a dry run until a named human
// approves it through the web app, which is the boundary this package exists to
// make visible (S6, S9).
package agent

import (
	"context"
	"errors"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"

	"github.com/GhandyP/project-pipeline-agent/internal/domain"
)

// AgentName is the name the ADK registers this agent under.
const AgentName = "pipeline_review_agent"

// instruction is the workflow the model is given. It states the safety rule
// where the model reads it, instead of relying on the tool descriptions alone.
const instruction = `You review one project pipeline and prepare a decision for a human. You never change a project.

Work in this order:
1. Call ` + ToolGetProjectStatus + ` with the project id. It returns the project snapshot, the blockers and overdue items that the deterministic rules derived, the project team, and the change fields you may propose.
2. Read the blockers. They are the ground truth: severity, owner state and evidence were computed by Go, not by you. Never invent a blocker, never re-score one, and never leave one out of your summary.
3. If you propose an update, call ` + ToolProposeUpdate + ` once with at most one change per field of one milestone. It returns a dry-run proposal and the intentions it refused. It writes nothing: no change is applied by any tool you can call.
4. Call ` + ToolSubmitReview + ` with a short summary and up to seven ranked next actions. Every action must cite at least one blocker id from step 1, and it may name an owner only from the project team.
5. Finish with one sentence a human can act on.

Rules you must respect:
- The findings were derived by deterministic Go rules from the snapshot. Report them as they are.
- A tracker update happens only after a human approves it outside this conversation. Never claim that a change was applied, saved, or written.
- If a tool refuses an intention, report the refusal instead of retrying it differently.
- If the project has no blockers, say so plainly instead of inventing work.`

// Actions is the slice of the service the tools need. Keeping it narrow lets
// the tools be tested against a fake that proves no write path is reachable.
type Actions interface {
	// Snapshot reads one project at its current version.
	Snapshot(ctx context.Context, projectID string) (domain.ProjectSnapshot, error)
	// Findings derives the rules' view of the project.
	Findings(ctx context.Context, projectID string) (domain.Findings, error)
	// Propose validates a change intention set and returns the dry-run update.
	Propose(ctx context.Context, projectID, rationale string, changes []domain.ModelChange) (domain.ProposedUpdate, []domain.Rejection, error)
	// Ground runs the model's answer through the deterministic gate.
	Ground(ctx context.Context, projectID string, review domain.ModelReview) (domain.PipelineReview, error)
}

// New builds the ADK agent over the pipeline actions.
//
// The returned agent is what the ADK console and the ADK web launcher mount. It
// has no write tool at all, so a model that decides to change a project has
// nowhere to send the request.
func New(llm model.LLM, actions Actions) (adkagent.Agent, error) {
	return newAgent(llm, actions, nil)
}

// newAgent builds the agent and optionally reports every grounded review it
// produced, which is how the streaming reviewer finds the review to emit.
func newAgent(llm model.LLM, actions Actions, collect func(domain.PipelineReview)) (adkagent.Agent, error) {
	if llm == nil {
		return nil, errNoModel
	}
	if actions == nil {
		return nil, errNoActions
	}
	tools, err := newTools(actions, collect)
	if err != nil {
		return nil, err
	}
	return llmagent.New(llmagent.Config{
		Name:        AgentName,
		Description: "Reviews one project pipeline: reads the status, explains the rules-derived blockers, and proposes a dry-run update for a human to approve.",
		Model:       llm,
		Instruction: instruction,
		Tools:       tools,
	})
}

// errNoModel and errNoActions are returned instead of failing later, inside a
// running agent.
var (
	errNoModel   = errors.New("agent: a language model is required")
	errNoActions = errors.New("agent: an actions implementation is required")
)
