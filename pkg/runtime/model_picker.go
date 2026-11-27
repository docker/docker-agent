package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/modelpicker"
)

// rootSessionKey is the context key holding the session of the root stream, so
// that tools running in a sub-session can record state on it.
type rootSessionKey struct{}

// findModelPickerTool returns the Tool from the current agent's
// toolsets, or nil if the agent has no model_picker configured.
func findModelPickerTool(a *agent.Agent) *modelpicker.ToolSet {
	for _, ts := range a.ToolSets() {
		if mpt, ok := tools.Find[*modelpicker.ToolSet](ts); ok {
			return mpt
		}
	}
	return nil
}

// handleChangeModel handles the change_model tool call by switching the current agent's model.
func (r *LocalRuntime) handleChangeModel(ctx context.Context, sess *session.Session, a *agent.Agent, toolCall tools.ToolCall, events EventSink) (*tools.ToolCallResult, error) {
	var params modelpicker.ChangeModelArgs
	if err := tools.UnmarshalToolArguments(ctx, toolCall, &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	if params.Model == "" {
		return tools.ResultError("model parameter is required"), nil
	}

	// Validate the requested model against the allowed list
	mpt := findModelPickerTool(a)
	if mpt == nil {
		return tools.ResultError("model_picker is not configured for this agent"), nil
	}
	allowed := mpt.AllowedModels()
	if !slices.Contains(allowed, params.Model) {
		return tools.ResultError(fmt.Sprintf(
			"model %q is not in the allowed list. Available models: %s",
			params.Model, strings.Join(allowed, ", "),
		)), nil
	}

	return r.setModelAndEmitInfo(ctx, sess, a, params.Model, events)
}

// handleRevertModel handles the revert_model tool call by reverting the current agent to its default model.
func (r *LocalRuntime) handleRevertModel(ctx context.Context, sess *session.Session, a *agent.Agent, events EventSink) (*tools.ToolCallResult, error) {
	return r.setModelAndEmitInfo(ctx, sess, a, "", events)
}

// setModelAndEmitInfo sets the model for the current agent and emits an updated
// AgentInfo event so the UI reflects the change. An empty modelRef reverts to
// the agent's default model.
func (r *LocalRuntime) setModelAndEmitInfo(ctx context.Context, sess *session.Session, a *agent.Agent, modelRef string, events EventSink) (*tools.ToolCallResult, error) {
	currentName := a.Name()
	if err := r.SetAgentModel(ctx, currentName, modelRef); err != nil {
		return tools.ResultError(fmt.Sprintf("failed to set model: %v", err)), nil
	}
	r.recordModelOverride(ctx, sess, currentName, modelRef)

	events.Emit(AgentInfo(a.Name(), r.getEffectiveModelID(ctx, a).String(), a.Description(), a.WelcomeMessage()))

	if modelRef == "" {
		slog.InfoContext(ctx, "Model reverted via model_picker tool", "agent", currentName)
		return tools.ResultSuccess("Model reverted to the agent's default model"), nil
	}
	slog.InfoContext(ctx, "Model changed via model_picker tool", "agent", currentName, "model", modelRef)
	return tools.ResultSuccess("Model changed to " + modelRef), nil
}

// recordModelOverride stores the override on the session that gets resumed, as
// the user's model picker does. The override applies to the whole team, so a
// switch made in a sub-session is recorded on the root session.
func (r *LocalRuntime) recordModelOverride(ctx context.Context, sess *session.Session, agentName, modelRef string) {
	if sess.IsSubSession() {
		root, ok := ctx.Value(rootSessionKey{}).(*session.Session)
		if !ok {
			return
		}
		sess = root
	}
	sess.SetAgentModelOverride(agentName, modelRef)
	if err := r.sessionStore.UpdateSession(context.WithoutCancel(ctx), sess); err != nil {
		slog.WarnContext(ctx, "Failed to persist model override", "session_id", sess.ID, "agent", agentName, "error", err)
	}
}
