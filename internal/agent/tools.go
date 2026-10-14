package agent

import (
	"errors"
	"fmt"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/GhandyP/project-pipeline-agent/internal/domain"
)

// Tool names. They are the contract the model sees and the tests assert, so
// they are exported constants instead of string literals scattered around.
const (
	// ToolGetProjectStatus reads one project and the rules-derived findings.
	ToolGetProjectStatus = "get_project_status"
	// ToolProposeUpdate validates a change intention set and returns a dry-run
	// update. It writes nothing.
	ToolProposeUpdate = "propose_update"
	// ToolSubmitReview runs the model's answer through the deterministic
	// grounding gate and returns the review a human reads.
	ToolSubmitReview = "submit_review"
)

// GetStatusArgs are the arguments of the read-only tool.
type GetStatusArgs struct {
	ProjectID string `json:"project_id" jsonschema:"The project id to review."`
}

// GetStatusResults is what the read-only tool returns: the snapshot, the
// derived findings, and the two pieces of reference data the model needs to
// stay inside the rules — the team it may assign from, and the fields it may
// propose changing.
type GetStatusResults struct {
	Snapshot            domain.ProjectSnapshot `json:"snapshot"`
	Findings            domain.Findings        `json:"findings"`
	Team                []domain.Owner         `json:"team"`
	AllowedChangeFields []string               `json:"allowed_change_fields"`
	MaxNextActions      int                    `json:"max_next_actions"`
}

// ProposeUpdateArgs is one change intention set, with the rationale a human
// will read next to the diff.
type ProposeUpdateArgs struct {
	ProjectID string               `json:"project_id" jsonschema:"The project id this update belongs to."`
	Rationale string               `json:"rationale" jsonschema:"One sentence explaining why this update is proposed."`
	Changes   []domain.ModelChange `json:"changes" jsonschema:"The field changes to validate: milestone_id, field and value."`
}

// ProposeUpdateResults is the dry run: what would change, or why nothing could.
type ProposeUpdateResults struct {
	Proposal   domain.ProposedUpdate `json:"proposal"`
	Rejections []domain.Rejection    `json:"rejections,omitempty"`
	// DryRun is always true. It is returned to the model so that no reading of
	// this result can suggest something was applied.
	DryRun bool `json:"dry_run"`
}

// SubmitReviewArgs is the model's answer: a summary, ranked actions that cite
// derived blockers, and optionally the same change intentions it proposed.
type SubmitReviewArgs struct {
	ProjectID   string               `json:"project_id" jsonschema:"The project id this review belongs to."`
	Summary     string               `json:"summary" jsonschema:"Two or three sentences naming the blockers that matter most."`
	NextActions []domain.ModelAction `json:"next_actions" jsonschema:"Ranked actions; each one must cite at least one blocker id from get_project_status."`
	Changes     []domain.ModelChange `json:"changes,omitempty" jsonschema:"The same change intentions passed to propose_update, if any."`
}

// SubmitReviewResults carries the grounded review.
type SubmitReviewResults struct {
	Review domain.PipelineReview `json:"review"`
}

// newTools builds the three tools in a fixed order. A nil collector leaves the
// tools unchanged: the ADK console path does not need to capture the review.
func newTools(actions Actions, collect func(domain.PipelineReview)) ([]tool.Tool, error) {
	configs := toolConfigs()
	built := make([]tool.Tool, 0, len(configs))
	for _, name := range []string{ToolGetProjectStatus, ToolProposeUpdate, ToolSubmitReview} {
		config, ok := configs[name]
		if !ok {
			return nil, fmt.Errorf("agent: no configuration is registered for tool %q", name)
		}
		var (
			registered tool.Tool
			err        error
		)
		switch name {
		case ToolGetProjectStatus:
			registered, err = functiontool.New(config, getProjectStatus(actions))
		case ToolProposeUpdate:
			registered, err = functiontool.New(config, proposeUpdate(actions))
		case ToolSubmitReview:
			registered, err = functiontool.New(config, submitReview(actions, collect))
		}
		if err != nil {
			return nil, fmt.Errorf("agent: build tool %q: %w", name, err)
		}
		built = append(built, registered)
	}
	return built, nil
}

// toolConfigs maps every tool name to the configuration the framework
// registers. It is the single source of truth newTools builds from, so a test
// that reads these configurations is reading the ones actually in use.
func toolConfigs() map[string]functiontool.Config {
	return map[string]functiontool.Config{
		ToolGetProjectStatus: {
			Name:        ToolGetProjectStatus,
			Description: "Read one project pipeline: the snapshot, the blockers and overdue items the deterministic rules derived, the project team and the change fields you may propose. Read-only; it changes nothing and needs no confirmation.",
		},
		ToolProposeUpdate: {
			Name:        ToolProposeUpdate,
			Description: "Validate a change intention set and return a dry-run update with the tracker version it was built for. It writes nothing: a human approves the update in the web app, and only then is it applied. Propose at most one change per field of one milestone.",
		},
		ToolSubmitReview: {
			Name:        ToolSubmitReview,
			Description: "Submit the review: a short summary, up to seven ranked next actions that each cite blocker ids returned by get_project_status, and the change intentions you proposed. The deterministic gate refuses an action that cites no real blocker and reports the refusal back to you.",
		},
	}
}

// getProjectStatus reads a project and its findings. It needs no confirmation
// because it writes nothing.
func getProjectStatus(actions Actions) functiontool.Func[GetStatusArgs, GetStatusResults] {
	return func(ctx adkagent.Context, args GetStatusArgs) (GetStatusResults, error) {
		snapshot, err := actions.Snapshot(ctx, args.ProjectID)
		if err != nil {
			return GetStatusResults{}, err
		}
		findings, err := actions.Findings(ctx, args.ProjectID)
		if err != nil {
			return GetStatusResults{}, err
		}
		fields := make([]string, 0, len(domain.AllChangeFields()))
		for _, field := range domain.AllChangeFields() {
			fields = append(fields, string(field))
		}
		return GetStatusResults{
			Snapshot:            snapshot,
			Findings:            findings,
			Team:                snapshot.Team,
			AllowedChangeFields: fields,
			MaxNextActions:      domain.MaxNextActions,
		}, nil
	}
}

// proposeUpdate validates a change intention set and returns the dry run.
//
// A refused intention is not an error: the model is told which intentions were
// refused and why, so it can submit an honest review instead of failing the
// whole run.
func proposeUpdate(actions Actions) functiontool.Func[ProposeUpdateArgs, ProposeUpdateResults] {
	return func(ctx adkagent.Context, args ProposeUpdateArgs) (ProposeUpdateResults, error) {
		proposal, rejections, err := actions.Propose(ctx, args.ProjectID, args.Rationale, args.Changes)
		if err != nil && !errors.Is(err, domain.ErrNoChanges) {
			return ProposeUpdateResults{}, err
		}
		return ProposeUpdateResults{Proposal: proposal, Rejections: rejections, DryRun: true}, nil
	}
}

// submitReview runs the model's answer through the grounding gate. A refusal
// inside the gate is part of the result, never an error.
func submitReview(actions Actions, collect func(domain.PipelineReview)) functiontool.Func[SubmitReviewArgs, SubmitReviewResults] {
	return func(ctx adkagent.Context, args SubmitReviewArgs) (SubmitReviewResults, error) {
		review, err := actions.Ground(ctx, args.ProjectID, domain.ModelReview{
			Summary:     args.Summary,
			NextActions: args.NextActions,
			Changes:     args.Changes,
		})
		if err != nil {
			return SubmitReviewResults{}, err
		}
		if collect != nil {
			collect(review)
		}
		return SubmitReviewResults{Review: review}, nil
	}
}
