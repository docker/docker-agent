package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/sessionplan"
)

func (r *LocalRuntime) handleWriteSessionPlan(ctx context.Context, _ *agent.Agent, sess *session.Session, toolCall tools.ToolCall, events EventSink, _ tools.Runtime) (*tools.ToolCallResult, error) {
	var args sessionplan.WriteSessionPlanArgs
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(args.Content) == "" {
		return tools.ResultError("content must not be empty"), nil
	}

	path, err := sessionplan.WriteContent(sessionplan.DefaultDir(), sess.ID, args.Content)
	if err != nil {
		if errors.Is(err, sessionplan.ErrInvalidSessionID) {
			return tools.ResultError(err.Error()), nil
		}
		return nil, err
	}

	events.Emit(SessionPlanUpdated(sess.ID, args.Content, path, r.CurrentAgentName(ctx)))
	return tools.ResultSuccess("Plan saved to " + path), nil
}

func (r *LocalRuntime) handleReadSessionPlan(_ context.Context, _ *agent.Agent, sess *session.Session, _ tools.ToolCall, _ EventSink, _ tools.Runtime) (*tools.ToolCallResult, error) {
	content, _, err := sessionplan.ReadContent(sessionplan.DefaultDir(), sess.ID)
	if errors.Is(err, sessionplan.ErrPlanNotFound) {
		return tools.ResultError("no plan written yet for this session; call write_session_plan first"), nil
	}
	if err != nil {
		if errors.Is(err, sessionplan.ErrInvalidSessionID) {
			return tools.ResultError(err.Error()), nil
		}
		return nil, err
	}
	return tools.ResultSuccess(content), nil
}

// handleExitPlanMode marks the session's plan as ready and returns control to
// the host. Switching agents is the host's decision — the runtime does not
// call setCurrentAgent here so a CLI that prints results inline, a chat UI
// with a mode toggle, and a server with a configured handoff can all consume
// the same marker without one stepping on the other.
func (r *LocalRuntime) handleExitPlanMode(_ context.Context, _ *agent.Agent, sess *session.Session, _ tools.ToolCall, _ EventSink, _ tools.Runtime) (*tools.ToolCallResult, error) {
	if _, _, err := sessionplan.ReadContent(sessionplan.DefaultDir(), sess.ID); err != nil {
		if errors.Is(err, sessionplan.ErrPlanNotFound) {
			return tools.ResultError("no plan to mark ready; call write_session_plan before exit_plan_mode"), nil
		}
		if errors.Is(err, sessionplan.ErrInvalidSessionID) {
			return tools.ResultError(err.Error()), nil
		}
		return nil, err
	}
	return tools.ResultSuccess("Plan ready for review."), nil
}
