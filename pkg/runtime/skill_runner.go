package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/skills"
)

// handleRunSkill executes a skill as an isolated sub-agent. The skill's
// SKILL.md content (with command expansions) becomes the system prompt, and
// the caller-provided task becomes the implicit user message. The sub-agent
// runs in a child session using the current agent's model and tools, and
// its final response is returned as the tool result.
//
// All skill-specific business rules (lookup, fork-mode validation, content
// expansion) live in (*skills.Toolset).PrepareForkSubSession; this
// handler keeps only the runtime-private orchestration that runForwarding
// can't generalise — namely the optional model override that applies for
// the sub-session's lifetime.
//
// This implements the `context: fork` behaviour from the SKILL.md frontmatter,
// following the same convention as Claude Code.
func (r *LocalRuntime) handleRunSkill(ctx context.Context, sess *session.Session, toolCall tools.ToolCall, evts EventSink) (*tools.ToolCallResult, error) {
	var args skills.RunSkillArgs
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	st := r.CurrentAgentSkillsToolset()
	if st == nil {
		return tools.ResultError("no skills are available for the current agent"), nil
	}

	prepared, errResult := st.PrepareForkSubSession(ctx, args)
	if errResult != nil {
		return errResult, nil
	}

	ca := r.CurrentAgentName()

	// Open the span before any pre-delegation work so model resolution
	// (inside WithAgentModel) is recorded under runtime.run_skill rather
	// than the parent session span.
	ctx, span := r.startSpan(ctx, "runtime.run_skill", trace.WithAttributes(
		attribute.String("agent", ca),
		attribute.String("skill", prepared.SkillName),
		attribute.String("session.id", sess.ID),
	))
	defer span.End()

	slog.DebugContext(ctx, "Running skill as sub-agent",
		"agent", ca,
		"skill", prepared.SkillName,
		"task", prepared.Task,
	)

	// If the skill declares a model override, apply it for the duration of
	// the sub-session. WithAgentModel handles every accepted form (named
	// model, alloy, inline provider/model, inline alloy) and returns a
	// CAS-safe restore func that is always non-nil; on failure we log a
	// warning and fall back to the agent's currently-active model.
	if prepared.Model != "" {
		restore, err := r.WithAgentModel(ctx, ca, prepared.Model)
		defer restore()
		if err != nil {
			slog.WarnContext(ctx, "Failed to apply skill model override; using current model",
				"agent", ca,
				"skill", prepared.SkillName,
				"model", prepared.Model,
				"error", err,
			)
		}
	}

	// run_skill keeps the same agent (skills are sub-sessions of the
	// caller, not delegations to another agent), so we never swap the
	// runtime's currentAgent here.
	return r.runForwarding(ctx, sess, evts, delegationRequest{
		SubSessionConfig: SubSessionConfig{
			Task:                prepared.Task,
			SystemMessage:       prepared.Content,
			ImplicitUserMessage: prepared.Task,
			AgentName:           ca,
			Title:               "Skill: " + prepared.SkillName,
			ToolsApproved:       sess.ToolsApproved,
			NonInteractive:      sess.NonInteractive,
			ExcludedTools:       []string{skills.ToolNameRunSkill},
		},
	})
}
