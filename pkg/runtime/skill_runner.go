package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/telemetry/genai"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/skills"
)

// handleRunSkill unmarshals the run_skill tool arguments and delegates to
// runSkillFork.
func (r *LocalRuntime) handleRunSkill(ctx context.Context, sess *session.Session, toolCall tools.ToolCall, evts EventSink, rt tools.Runtime) (*tools.ToolCallResult, error) {
	var args skills.RunSkillArgs
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	return r.runSkillFork(ctx, sess, args, evts, rt)
}

// RunSkillFork executes a `context: fork` skill as an isolated sub-session.
// The expanded SKILL.md body becomes the child's first user message; the
// agent's own system prompt is preserved. Entry point for callers outside a
// tool call (the App's slash-command path); run_skill goes through
// [LocalRuntime.handleRunSkill].
func (r *LocalRuntime) RunSkillFork(ctx context.Context, sess *session.Session, args skills.RunSkillArgs, evts EventSink) (*tools.ToolCallResult, error) {
	return r.runSkillFork(ctx, sess, args, evts, nil)
}

// runSkillFork prepares and runs the fork sub-session. rt is the in-flight
// tool call's runtime handle. Standalone invocations pass nil and skip embedded
// commands because no tool call exists to own an approval prompt.
func (r *LocalRuntime) runSkillFork(ctx context.Context, sess *session.Session, args skills.RunSkillArgs, evts EventSink, rt tools.Runtime) (*tools.ToolCallResult, error) {
	// The caller never resolves from the shared current agent: a fork skill
	// invoked from a pinned background session must use the pinned agent's
	// skills, identity, and model override (#3886), and a sibling transfer_task
	// in the same parallel batch may already have swapped it — which would run
	// the skill as the wrong agent (#4156). rt is nil for standalone
	// invocations; callerAgent falls back to session resolution there.
	caller := r.callerAgent(rt, sess)
	if caller == nil {
		return nil, errors.New("no agent resolved for the calling session")
	}
	ca := caller.Name()

	st := agentSkillsToolset(caller)
	if st == nil {
		return tools.ResultError("no skills are available for agent " + ca), nil
	}

	if rt == nil {
		rt = tools.NopRuntime{}
	}

	prepared, errResult, err := st.PrepareForkSubSession(ctx, args, rt)
	if err != nil {
		return nil, err
	}
	if errResult != nil {
		return errResult, nil
	}

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
	// runtime's currentAgent stays put and the delegation lineage is
	// inherited unchanged (no DelegationLineage: not a delegation edge).
	// When the caller session is itself pinned (a background agent's
	// session), pin the child to the same agent so RunStream resolves it
	// as the pinned caller instead of the shared current agent.
	return r.runForwarding(ctx, sess, evts, delegationRequest{
		CallerAgent: caller,
		SubSessionConfig: SubSessionConfig{
			Task:                prepared.Task,
			SystemMessage:       skills.BuildSkillSystemMessage(prepared, sess.AttachedFilesSnapshot()),
			ImplicitUserMessage: skills.BuildSkillUserMessage(prepared),
			AgentName:           ca,
			Title:               "Skill: " + prepared.SkillName,
			ToolsApproved:       sess.IsToolsApproved(),
			SafetyPolicy:        sess.GetSafetyPolicy(),
			Permissions:         sess.ClonePermissions(),
			NonInteractive:      sess.NonInteractive,
			PinAgent:            sess.AgentName != "",
			ExcludedTools:       []string{skills.ToolNameRunSkill},
			AllowedTools:        prepared.AllowedTools,
			ExtraToolSets:       prepared.ToolSets,
			// A fork skill's answer is free-form text consumed by the calling
			// agent, not the agent's schema-constrained final output, so the
			// child is exempt from tool-mode structured-output enforcement.
			DisableStructuredOutput: true,
		},
	})
}
