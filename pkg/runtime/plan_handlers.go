package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/plan"
)

func (r *LocalRuntime) handleWritePlan(ctx context.Context, sess *session.Session, toolCall tools.ToolCall, events EventSink) (*tools.ToolCallResult, error) {
	var args plan.WritePlanArgs
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(args.Content) == "" {
		return tools.ResultError("content must not be empty"), nil
	}

	path, err := plan.WriteContent(plan.DefaultDir(), sess.ID, args.Content)
	if err != nil {
		if errors.Is(err, plan.ErrInvalidSessionID) {
			return tools.ResultError(err.Error()), nil
		}
		return nil, err
	}

	events.Emit(PlanUpdated(sess.ID, args.Content, path, r.CurrentAgentName(ctx)))
	return tools.ResultSuccess("Plan saved to " + path), nil
}

func (r *LocalRuntime) handleReadPlan(_ context.Context, sess *session.Session, _ tools.ToolCall, _ EventSink) (*tools.ToolCallResult, error) {
	content, _, err := plan.ReadContent(plan.DefaultDir(), sess.ID)
	if errors.Is(err, plan.ErrPlanNotFound) {
		return tools.ResultError("no plan written yet for this session; call write_plan first"), nil
	}
	if err != nil {
		if errors.Is(err, plan.ErrInvalidSessionID) {
			return tools.ResultError(err.Error()), nil
		}
		return nil, err
	}
	return tools.ResultSuccess(content), nil
}

// handleExitPlanMode marks the session's plan as ready and returns control to
// the user. Switching agents is a separate signal (the host application picks
// the agent for the next turn) so we deliberately do not call setCurrentAgent
// here; doing so would force a handoff the user hasn't actually opted into.
func (r *LocalRuntime) handleExitPlanMode(_ context.Context, sess *session.Session, _ tools.ToolCall, _ EventSink) (*tools.ToolCallResult, error) {
	if _, _, err := plan.ReadContent(plan.DefaultDir(), sess.ID); err != nil {
		if errors.Is(err, plan.ErrPlanNotFound) {
			return tools.ResultError("no plan to mark ready; call write_plan before exit_plan_mode"), nil
		}
		return nil, err
	}
	return tools.ResultSuccess("Plan ready for review."), nil
}
