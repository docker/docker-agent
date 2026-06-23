package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/telemetry/genai"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/skills"
)

// handleRunSkill unmarshals the run_skill tool arguments and delegates
// to RunSkillFork.
func (r *LocalRuntime) handleRunSkill(ctx context.Context, sess *session.Session, toolCall tools.ToolCall, evts EventSink) (*tools.ToolCallResult, error) {
	var args skills.RunSkillArgs
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	return r.RunSkillFork(ctx, sess, args, evts)
}

// RunSkillFork executes a `context: fork` skill as an isolated sub-session.
// The expanded SKILL.md body becomes the child's first user message; the
// agent's own system prompt is preserved. Shared by the run_skill tool
// and the App's slash-command path.
func (r *LocalRuntime) RunSkillFork(ctx context.Context, sess *session.Session, args skills.RunSkillArgs, evts EventSink) (*tools.ToolCallResult, error) {
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
	//
	// Skills are workflow-shaped (a coordinated process the agent
	// orchestrates), so the GenAI semconv `invoke_workflow` operation
	// applies. Emit it via gen_ai.* attrs alongside the legacy keys
	// for back-compat.
	skillAttrs := []attribute.KeyValue{
		attribute.String(genai.AttrOperationName, genai.OperationInvokeWorkflow),
		attribute.String(genai.AttrWorkflowName, prepared.SkillName),
		attribute.String(genai.AttrAgentNameRuntime, ca),
		attribute.String(genai.AttrConversationID, sess.ID),
	}
	if genai.EmitLegacyAttributes() {
		skillAttrs = append(skillAttrs,
			attribute.String("agent", ca),
			attribute.String("skill", prepared.SkillName),
			attribute.String("session.id", sess.ID),
		)
	}
	// Span name follows the GenAI agent semconv pattern
	// `invoke_workflow {workflow.name}` so spec-aware backends
	// classify the span as a workflow invocation. SpanKindInternal is
	// passed explicitly per spec rather than relying on the SDK
	// default — keeps intent clear and immune to default changes.
	spanName := genai.OperationInvokeWorkflow
	if prepared.SkillName != "" {
		spanName = genai.OperationInvokeWorkflow + " " + prepared.SkillName
	}
	ctx, span := r.startSpan(ctx, spanName,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(skillAttrs...),
	)
	defer span.End()

	slog.DebugContext(ctx, "Running skill as sub-agent",
		"agent", ca,
		"skill", prepared.SkillName,
		"task", prepared.Task,
	)

	// Apply the skill's optional model override for the sub-session.
	// On failure we log and fall back to the agent's current model;
	// restore is CAS-safe and always non-nil.
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

	// Skills are sub-sessions of the caller, not delegations, so the
	// runtime's currentAgent stays put.
	return r.runForwarding(ctx, sess, evts, delegationRequest{
		SubSessionConfig: SubSessionConfig{
			Task:                prepared.Task,
			SystemMessage:       skills.BuildSkillSystemMessage(prepared, sess.AttachedFilesSnapshot()),
			ImplicitUserMessage: skills.BuildSkillUserMessage(prepared),
			AgentName:           ca,
			Title:               "Skill: " + prepared.SkillName,
			ToolsApproved:       sess.ToolsApproved,
			NonInteractive:      sess.NonInteractive,
			ExcludedTools:       []string{skills.ToolNameRunSkill},
		},
	})
}
