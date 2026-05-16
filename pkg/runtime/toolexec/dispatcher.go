package toolexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/telemetry"
	"github.com/docker/docker-agent/pkg/tools"
)

// Verdicts and sources surfaced via [HookDispatcher.NotifyApprovalDecision].
// The strings are part of the on_tool_approval_decision hook contract and
// must stay stable.
const (
	ApprovalDecisionAllow    = "allow"
	ApprovalDecisionDeny     = "deny"
	ApprovalDecisionCanceled = "canceled"

	ApprovalSourceYolo                    = "yolo"
	ApprovalSourceSessionPermissionsAllow = "session_permissions_allow"
	ApprovalSourceSessionPermissionsDeny  = "session_permissions_deny"
	ApprovalSourceTeamPermissionsAllow    = "team_permissions_allow"
	ApprovalSourceTeamPermissionsDeny     = "team_permissions_deny"
	ApprovalSourcePreToolUseHookAllow     = "pre_tool_use_hook_allow"
	ApprovalSourcePreToolUseHookDeny      = "pre_tool_use_hook_deny"
	ApprovalSourceReadOnlyHint            = "readonly_hint"
	ApprovalSourceUserApproved            = "user_approved"
	ApprovalSourceUserApprovedSession     = "user_approved_session"
	ApprovalSourceUserApprovedTool        = "user_approved_tool"
	ApprovalSourceUserRejected            = "user_rejected"
	ApprovalSourceContextCanceled         = "context_canceled"
)

// CallOutcome captures the verdicts of a single tool invocation as
// observed by the dispatcher.
//
// Canceled and StopRun are mutually exclusive in practice but signal
// different things to the caller: cancellation halts the current batch
// silently (the run loop continues so the synthesised tool error
// responses can be sent back to the model on the next turn); StopRun
// also terminates the agent's run loop with a user-visible reason
// produced by a post_tool_use hook deny verdict.
type CallOutcome struct {
	Canceled    bool
	StopRun     bool
	StopMessage string
}

// Emitter receives the events the [Dispatcher] emits while processing a
// batch of tool calls. Runtimes typically implement this by sending typed
// events to their event channel.
//
// The dispatcher only emits the five events below. Runtime-managed
// handlers (registered via [Dispatcher.Handlers]) emit any additional
// runtime-specific events directly via the channel they captured at
// registration time.
type Emitter interface {
	EmitToolCall(toolCall tools.ToolCall, tool tools.Tool, agentName string)
	EmitToolCallResponse(toolCallID string, tool tools.Tool, result *tools.ToolCallResult, output, agentName string)
	EmitToolCallConfirmation(toolCall tools.ToolCall, tool tools.Tool, agentName string)
	EmitHookBlocked(toolCall tools.ToolCall, tool tools.Tool, message, agentName string)
	EmitMessageAdded(sessionID string, msg *session.Message, agentName string)
}

// HookDispatcher abstracts pre/post tool-use hook dispatch and the
// "user is being prompted" notification.
type HookDispatcher interface {
	// Dispatch fires a tool-related hook (typically [hooks.EventPreToolUse]
	// or [hooks.EventPostToolUse]). Returning nil is the "carry on with the
	// original call" signal — used uniformly when no hook is configured,
	// the agent is missing, or dispatch failed.
	Dispatch(ctx context.Context, a *agent.Agent, event hooks.EventType, in *hooks.Input) *hooks.Result

	// NotifyUserInput is invoked just before the dispatcher blocks waiting
	// for the user (tool confirmation). Implementations typically fire
	// [hooks.EventOnUserInput].
	NotifyUserInput(ctx context.Context, sessionID, label string)

	// NotifyApprovalDecision is invoked once per tool call after the
	// approval pipeline (auto-allow, deny, user confirmation, ...) has
	// resolved a verdict. Implementations typically fire
	// [hooks.EventOnToolApprovalDecision] with decision and source set
	// to the supplied strings (see ApprovalDecision* / ApprovalSource*
	// constants).
	NotifyApprovalDecision(ctx context.Context, sess *session.Session, a *agent.Agent, tc tools.ToolCall, decision, source string)
}

// ToolHandler is the signature for runtime-managed tool handlers
// (e.g. transfer_task, handoff, change_model). The dispatcher wraps every
// handler in tracing/telemetry/event-emission, so handlers MUST NOT emit
// ToolCall/ToolCallResponse themselves. Handlers that need to emit other
// event types should be wired by the caller to capture the relevant
// channel via closure when registering the handler.
type ToolHandler func(ctx context.Context, sess *session.Session, tc tools.ToolCall) (*tools.ToolCallResult, error)

// ResumeRequest carries the user's response to a tool-confirmation prompt.
// The runtime aliases this type publicly via runtime.ResumeRequest so the
// dispatcher and the runtime share one definition.
type ResumeRequest struct {
	Type     ResumeType
	Reason   string // Optional; primarily used with [ResumeTypeReject]
	ToolName string // Optional; used with [ResumeTypeApproveTool]
}

// ResumeType identifies the kind of confirmation a user responded with.
type ResumeType string

const (
	ResumeTypeApprove        ResumeType = "approve"
	ResumeTypeApproveSession ResumeType = "approve-session"
	ResumeTypeApproveTool    ResumeType = "approve-tool"
	ResumeTypeReject         ResumeType = "reject"
)

// Dispatcher executes batches of tool calls. Construct one per runtime
// (or per RunStream) and call [Dispatcher.Process] for each LLM response.
// The dispatcher is goroutine-safe only insofar as its dependencies are.
type Dispatcher struct {
	// Tracer records per-call spans. May be nil (no-op tracing).
	Tracer trace.Tracer

	// Hooks dispatches pre/post tool-use hooks. May be nil for runtimes
	// without hook support; in that case every call runs unchanged.
	Hooks HookDispatcher

	// Resume receives user-confirmation responses. Must be set; the
	// dispatcher blocks on it whenever a tool requires confirmation.
	Resume <-chan ResumeRequest

	// AgentFor returns the active agent for a session. Required.
	AgentFor func(*session.Session) *agent.Agent

	// Permissions returns the ordered list of permission checkers for a
	// session (typically session-level first, then team-level). May be
	// nil; treated the same as returning an empty slice.
	Permissions func(*session.Session) []NamedChecker

	// Handlers maps tool names to runtime-managed handlers (transfer_task,
	// handoff, change_model, ...). Tools not in this map are routed to
	// their toolset Handler.
	Handlers map[string]ToolHandler
}

// Process runs every tool call in calls in order, emitting events through
// em.
//
// Returns (stopRun, message) when a post_tool_use hook signalled a
// terminating verdict during this batch; the run loop then fans out the
// standard Error / notification / on_error stanzas before exiting.
// (false, "") in every other path — including user cancellation, which
// halts the *batch* but keeps the loop alive so the synthesised tool
// error responses can be sent back to the model on the next turn.
func (d *Dispatcher) Process(ctx context.Context, sess *session.Session, calls []tools.ToolCall, agentTools []tools.Tool, em Emitter) (stopRun bool, stopMessage string) {
	a := d.AgentFor(sess)
	slog.DebugContext(ctx, "Processing tool calls", "agent", a.Name(), "call_count", len(calls))

	toolByName := make(map[string]tools.Tool, len(agentTools))
	for _, t := range agentTools {
		toolByName[t.Name] = t
	}

	// synthesizeRemaining adds error responses for tool calls we won't
	// run because the batch was halted (user cancellation or post-tool
	// stopRun). Orphan function calls without matching outputs are
	// rejected by the Responses API, so we surface them as errors
	// rather than dropping them.
	synthesizeRemaining := func(remaining []tools.ToolCall, reason string) {
		for _, rc := range remaining {
			c := d.newCall(sess, em, a, rc, toolByName)
			c.errorResponse(ctx, reason)
		}
	}

	for i, tc := range calls {
		c := d.newCall(sess, em, a, tc, toolByName)
		outcome := c.run(ctx)
		switch {
		case outcome.Canceled:
			synthesizeRemaining(calls[i+1:],
				"The tool call was canceled because a previous tool call in the same batch was canceled by the user.")
			return false, ""
		case outcome.StopRun:
			synthesizeRemaining(calls[i+1:],
				"The tool call was skipped because a post_tool_use hook signalled run termination.")
			return true, outcome.StopMessage
		}
	}
	return false, ""
}

// newCall assembles a [call] for a single tool invocation, looking up the
// referenced tool in the agent's toolset. When the tool isn't found, the
// call is marked unavailable and tool.Name is set to the requested name
// so error events still carry a meaningful label.
func (d *Dispatcher) newCall(sess *session.Session, em Emitter, a *agent.Agent, tc tools.ToolCall, toolByName map[string]tools.Tool) *call {
	tool, available := toolByName[tc.Function.Name]
	if !available {
		tool = tools.Tool{Name: tc.Function.Name}
	}
	return &call{
		d:         d,
		sess:      sess,
		em:        em,
		a:         a,
		tc:        tc,
		tool:      tool,
		available: available,
	}
}

// call bundles the per-tool-call state used by the dispatcher's helpers.
// Carrying it as a single value cuts the helpers' parameter lists from
// 7-8 arguments down to a method receiver, and groups the mutable state
// (pre-hook may rewrite tc.Function.Arguments) in one place.
//
// ctx is intentionally NOT a field: storing context.Context in a struct
// is a documented Go anti-pattern (it hides cancellation flow). Methods
// that need ctx accept it explicitly as the first argument.
type call struct {
	d    *Dispatcher
	sess *session.Session
	em   Emitter
	a    *agent.Agent

	tc        tools.ToolCall // mutable: pre_tool_use hooks may rewrite arguments
	tool      tools.Tool     // tool.Name is always set; other fields zero when !available
	available bool           // false when the tool wasn't in the agent's toolset
}

// run processes a single tool call and returns its outcome. All span
// and approval bookkeeping lives here so the call lifecycle is visible
// at a glance.
func (c *call) run(ctx context.Context) CallOutcome {
	ctx, span := c.d.startSpan(ctx, "runtime.tool.call", trace.WithAttributes(
		attribute.String("tool.name", c.tc.Function.Name),
		attribute.String("tool.type", string(c.tc.Type)),
		attribute.String("agent", c.a.Name()),
		attribute.String("session.id", c.sess.ID),
		attribute.String("tool.call_id", c.tc.ID),
	))
	defer span.End()

	slog.DebugContext(ctx, "Processing tool call", "agent", c.a.Name(), "tool", c.tc.Function.Name, "session_id", c.sess.ID)

	// After a handoff the model may hallucinate tools it saw earlier in
	// the conversation. Reject unknown tools with an error response so it
	// can self-correct.
	if !c.available {
		slog.WarnContext(ctx, "Tool call for unavailable tool", "agent", c.a.Name(), "tool", c.tc.Function.Name, "session_id", c.sess.ID)
		c.errorResponse(ctx, fmt.Sprintf("Tool '%s' is not available. You can only use the tools provided to you.", c.tc.Function.Name))
		span.SetStatus(codes.Error, "tool not available")
		return CallOutcome{}
	}

	// Pick the deferred work that runs once approval clears: runtime-managed
	// tools (transfer_task, handoff) have dedicated handlers; everything
	// else goes through the toolset.
	var runTool func() CallOutcome
	if handler, ok := c.d.Handlers[c.tc.Function.Name]; ok {
		runTool = func() CallOutcome {
			c.runHandler(ctx, handler)
			return CallOutcome{}
		}
	} else {
		runTool = func() CallOutcome {
			return c.runToolset(ctx)
		}
	}

	outcome := c.approveAndRun(ctx, runTool)
	if outcome.Canceled {
		span.SetStatus(codes.Ok, "tool call canceled by user")
	} else {
		span.SetStatus(codes.Ok, "tool call processed")
	}
	return outcome
}

// approveAndRun runs runTool if the configured approval pipeline allows
// it, otherwise records an error or asks the user.
//
// The pipeline order is:
//
//  1. yolo / permission checkers (delegated to [Decide]) — deterministic
//     verdicts win first. ForceAsk goes straight to the user.
//  2. pre_tool_use hooks (LLM-judge, shell scripts, ...) — consulted
//     ONLY when no deterministic checker matched. The hook can Deny
//     (block), Allow (skip the user prompt) or Ask (force the prompt).
//     Hooks may also rewrite tool arguments via UpdatedInput, in which
//     case the rewrite is applied here so the user prompt and the tool
//     handler both see the modified call.
//  3. read-only hint — auto-approve when the tool advertises it.
//  4. user confirmation — fallback prompt.
//
// Splitting the read-only hint out of [Decide] is deliberate: it lets
// the pre_tool_use hook see (and override) calls that would otherwise
// auto-approve via the read-only hint. This matches the PR's intent
// that an LLM judge gets a turn on every call that isn't covered by an
// explicit allow/deny rule.
func (c *call) approveAndRun(ctx context.Context, runTool func() CallOutcome) CallOutcome {
	var checkers []NamedChecker
	if c.d.Permissions != nil {
		checkers = c.d.Permissions(c.sess)
	}

	// readOnlyHint is intentionally false here so the pre_tool_use hook
	// gets a turn before the read-only fast-path applies.
	decision := Decide(
		c.sess.ToolsApproved,
		checkers,
		c.tc.Function.Name,
		ParseToolInput(c.tc.Function.Arguments),
		false,
	)

	switch decision.Outcome {
	case OutcomeAllow:
		c.logAllow(decision)
		c.notifyApproval(ctx, ApprovalDecisionAllow, allowSourceForDecision(decision))
		return runTool()
	case OutcomeDeny:
		slog.DebugContext(ctx, "Tool denied by permissions", "tool", c.tc.Function.Name, "source", decision.Source, "session_id", c.sess.ID)
		c.notifyApproval(ctx, ApprovalDecisionDeny, denySourceForChecker(decision.Source))
		c.errorResponse(ctx, fmt.Sprintf("Tool '%s' is denied by %s.", c.tc.Function.Name, decision.Source))
		return CallOutcome{}
	case OutcomeAsk:
		if decision.Reason == ReasonChecker {
			// Explicit ask pattern from a checker: skip the hook and
			// prompt the user directly. The user is the source of
			// truth for these calls.
			slog.DebugContext(ctx, "Tool requires confirmation (ask pattern)", "tool", c.tc.Function.Name, "source", decision.Source, "session_id", c.sess.ID)
			return c.askUser(ctx, runTool)
		}
	}

	// No deterministic verdict: consult the pre_tool_use hook chain.
	if outcome, handled := c.consultPreToolUseHook(ctx, runTool); handled {
		return outcome
	}

	if c.tool.Annotations.ReadOnlyHint {
		c.notifyApproval(ctx, ApprovalDecisionAllow, ApprovalSourceReadOnlyHint)
		return runTool()
	}
	return c.askUser(ctx, runTool)
}

// consultPreToolUseHook fires the pre_tool_use hook chain in the
// approval flow, before the user is asked.
//
// Returns (outcome, true) when the hook produced a definitive verdict
// (Deny / Allow / Ask) that the caller should honor; returns
// (zero, false) when no hook is configured or the chain returned no
// opinion, in which case the caller should fall through to the
// read-only hint / user prompt.
//
// UpdatedInput from a hook is applied to c.tc here so every downstream
// path (auto-run, user prompt, runToolset) sees the rewritten
// arguments — this is the only place pre-call argument rewriting
// happens.
func (c *call) consultPreToolUseHook(ctx context.Context, runTool func() CallOutcome) (CallOutcome, bool) {
	if c.d.Hooks == nil {
		return CallOutcome{}, false
	}

	result := c.d.Hooks.Dispatch(ctx, c.a, hooks.EventPreToolUse, NewHooksInput(c.sess, c.tc))
	if result == nil {
		return CallOutcome{}, false
	}

	// Apply UpdatedInput first so subsequent paths see the rewritten args.
	c.applyHookModifiedInput(result)

	if !result.Allowed {
		slog.DebugContext(ctx, "Pre-tool hook blocked tool call", "tool", c.tc.Function.Name, "message", result.Message)
		c.notifyApproval(ctx, ApprovalDecisionDeny, ApprovalSourcePreToolUseHookDeny)
		c.em.EmitHookBlocked(c.tc, c.tool, result.Message, c.a.Name())
		c.errorResponse(ctx, "Tool call blocked by hook: "+result.Message)
		return CallOutcome{}, true
	}

	switch result.Decision {
	case hooks.DecisionAllow:
		slog.DebugContext(ctx, "Tool auto-approved by pre_tool_use hook", "tool", c.tc.Function.Name, "reason", result.DecisionReason, "session_id", c.sess.ID)
		c.notifyApproval(ctx, ApprovalDecisionAllow, ApprovalSourcePreToolUseHookAllow)
		return runTool(), true
	case hooks.DecisionAsk:
		slog.DebugContext(ctx, "pre_tool_use hook escalated to user", "tool", c.tc.Function.Name, "reason", result.DecisionReason, "session_id", c.sess.ID)
		return c.askUser(ctx, runTool), true
	}
	return CallOutcome{}, false
}

// applyHookModifiedInput applies a hook's UpdatedInput to the in-flight
// tool call. Errors are logged at warn level and otherwise ignored —
// the hook can't crash the call by returning malformed JSON.
func (c *call) applyHookModifiedInput(result *hooks.Result) {
	if result.ModifiedInput == nil {
		return
	}
	updated, err := json.Marshal(result.ModifiedInput)
	if err != nil {
		slog.Warn("Failed to marshal modified tool input from hook", "tool", c.tc.Function.Name, "error", err)
		return
	}
	slog.Debug("Pre-tool hook modified tool input", "tool", c.tc.Function.Name)
	c.tc.Function.Arguments = string(updated)
}

// notifyApproval forwards the resolved approval decision to the
// HookDispatcher, when one is configured. Centralised so the nil-guard
// stays in one place.
func (c *call) notifyApproval(ctx context.Context, decision, source string) {
	if c.d.Hooks == nil {
		return
	}
	c.d.Hooks.NotifyApprovalDecision(ctx, c.sess, c.a, c.tc, decision, source)
}

// logAllow emits the auto-approval debug log appropriate to the reason
// that produced the [OutcomeAllow] decision.
func (c *call) logAllow(d PermissionDecision) {
	switch d.Reason {
	case ReasonYolo:
		slog.Debug("Tool auto-approved by --yolo flag", "tool", c.tc.Function.Name, "session_id", c.sess.ID)
	case ReasonChecker:
		slog.Debug("Tool auto-approved by permissions", "tool", c.tc.Function.Name, "source", d.Source, "session_id", c.sess.ID)
		// ReasonReadOnlyHint is intentionally silent (matches prior behaviour).
	}
}

// allowSourceForDecision maps a [PermissionDecision] with [OutcomeAllow]
// onto the corresponding ApprovalSource* constant.
func allowSourceForDecision(d PermissionDecision) string {
	switch d.Reason {
	case ReasonYolo:
		return ApprovalSourceYolo
	case ReasonReadOnlyHint:
		return ApprovalSourceReadOnlyHint
	case ReasonChecker:
		return allowSourceForChecker(d.Source)
	}
	return allowSourceForChecker(d.Source)
}

// allowSourceForChecker maps a checker source label ("session permissions"
// or "permissions configuration") onto the corresponding ApprovalSource*
// allow constant.
func allowSourceForChecker(checkerSource string) string {
	if checkerSource == "session permissions" {
		return ApprovalSourceSessionPermissionsAllow
	}
	return ApprovalSourceTeamPermissionsAllow
}

// denySourceForChecker mirrors allowSourceForChecker for the deny path.
func denySourceForChecker(checkerSource string) string {
	if checkerSource == "session permissions" {
		return ApprovalSourceSessionPermissionsDeny
	}
	return ApprovalSourceTeamPermissionsDeny
}

// askUser sends a confirmation event and waits for the user's response
// on the resume channel or for ctx cancellation. Only called when no
// permission rule auto-approved the tool.
//
// permission_request hooks fire first and may short-circuit the prompt
// with an explicit allow or deny verdict; returning nothing falls
// through to the interactive confirmation.
func (c *call) askUser(ctx context.Context, runTool func() CallOutcome) CallOutcome {
	if outcome, handled := c.runPermissionRequestHook(ctx, runTool); handled {
		return outcome
	}

	slog.DebugContext(ctx, "Tools not approved, waiting for resume", "tool", c.tc.Function.Name, "session_id", c.sess.ID)
	c.em.EmitToolCallConfirmation(c.tc, c.tool, c.a.Name())

	if c.d.Hooks != nil {
		c.d.Hooks.NotifyUserInput(ctx, c.sess.ID, "tool confirmation")
	}

	select {
	case req := <-c.d.Resume:
		return c.handleResume(ctx, req, runTool)
	case <-ctx.Done():
		slog.DebugContext(ctx, "Context cancelled while waiting for resume", "tool", c.tc.Function.Name, "session_id", c.sess.ID)
		c.notifyApproval(ctx, ApprovalDecisionCanceled, ApprovalSourceContextCanceled)
		c.errorResponse(ctx, "The tool call was canceled by the user.")
		return CallOutcome{Canceled: true}
	}
}

// runPermissionRequestHook dispatches the permission_request hook just
// before the runtime would prompt the user for confirmation. The hook
// can short-circuit the prompt by returning permission_decision
// ("allow" or "deny") in hook_specific_output. A bare deny (Decision=
// "block" without permission_decision) is also honoured. Returning
// nothing keeps the existing behaviour and asks the user.
func (c *call) runPermissionRequestHook(ctx context.Context, runTool func() CallOutcome) (CallOutcome, bool) {
	if c.d.Hooks == nil {
		return CallOutcome{}, false
	}

	toolName := c.tc.Function.Name
	result := c.d.Hooks.Dispatch(ctx, c.a, hooks.EventPermissionRequest, &hooks.Input{
		SessionID: c.sess.ID,
		ToolName:  toolName,
		ToolUseID: c.tc.ID,
		ToolInput: ParseToolInput(c.tc.Function.Arguments),
	})
	if result == nil {
		return CallOutcome{}, false
	}

	if !result.Allowed {
		slog.DebugContext(ctx, "Tool denied by permission_request hook", "tool", toolName, "session_id", c.sess.ID, "reason", result.Message)
		rejectMsg := "The tool call was rejected by a permission_request hook."
		if reason := strings.TrimSpace(result.Message); reason != "" {
			rejectMsg += " Reason: " + reason
		}
		c.errorResponse(ctx, rejectMsg)
		return CallOutcome{}, true
	}

	if result.PermissionAllowed {
		slog.DebugContext(ctx, "Tool auto-approved by permission_request hook", "tool", toolName, "session_id", c.sess.ID, "reason", result.AdditionalContext)
		return runTool(), true
	}

	return CallOutcome{}, false
}

// handleResume applies the user's confirmation decision: run the tool
// (with optional session/tool-wide approval persistence) or emit a
// rejection error response.
func (c *call) handleResume(ctx context.Context, req ResumeRequest, runTool func() CallOutcome) CallOutcome {
	switch req.Type {
	case ResumeTypeApprove:
		slog.DebugContext(ctx, "Resume signal received, approving tool", "tool", c.tc.Function.Name, "session_id", c.sess.ID)
		c.notifyApproval(ctx, ApprovalDecisionAllow, ApprovalSourceUserApproved)
		return runTool()
	case ResumeTypeApproveSession:
		slog.DebugContext(ctx, "Resume signal received, approving session", "tool", c.tc.Function.Name, "session_id", c.sess.ID)
		c.sess.ToolsApproved = true
		c.notifyApproval(ctx, ApprovalDecisionAllow, ApprovalSourceUserApprovedSession)
		return runTool()
	case ResumeTypeApproveTool:
		approvedTool := req.ToolName
		if approvedTool == "" {
			approvedTool = c.tc.Function.Name
		}
		if c.sess.Permissions == nil {
			c.sess.Permissions = &session.PermissionsConfig{}
		}
		if !slices.Contains(c.sess.Permissions.Allow, approvedTool) {
			c.sess.Permissions.Allow = append(c.sess.Permissions.Allow, approvedTool)
		}
		slog.DebugContext(ctx, "Resume signal received, approving tool permanently", "tool", approvedTool, "session_id", c.sess.ID)
		c.notifyApproval(ctx, ApprovalDecisionAllow, ApprovalSourceUserApprovedTool)
		return runTool()
	case ResumeTypeReject:
		slog.DebugContext(ctx, "Resume signal received, rejecting tool", "tool", c.tc.Function.Name, "session_id", c.sess.ID, "reason", req.Reason)
		c.notifyApproval(ctx, ApprovalDecisionDeny, ApprovalSourceUserRejected)
		msg := "The user rejected the tool call."
		if reason := strings.TrimSpace(req.Reason); reason != "" {
			msg += " Reason: " + reason
		}
		c.errorResponse(ctx, msg)
	}
	return CallOutcome{}
}

// runToolset executes a tool from an agent's toolset (MCP, filesystem, ...),
// surrounding the call with the post-tool-use hook. The pre-tool-use
// hook fires earlier in [call.approveAndRun] (so an LLM-judge can
// short-circuit the user prompt); by the time we get here, any
// argument rewrite the hook requested has already been applied to
// c.tc. The post-tool-use hook may signal run termination via its
// returned [CallOutcome].
func (c *call) runToolset(ctx context.Context) CallOutcome {
	res := c.invoke(ctx, "runtime.tool.handler", func(ctx context.Context) (*tools.ToolCallResult, time.Duration, error) {
		res, err := c.tool.Handler(ctx, c.tc)
		return res, 0, err
	})

	stop, msg := c.postHook(ctx, res)
	return CallOutcome{StopRun: stop, StopMessage: msg}
}

// runHandler executes a runtime-managed tool handler. Hooks do not fire
// for runtime-managed handlers — they're internal plumbing, not user-
// configurable tools.
func (c *call) runHandler(ctx context.Context, handler ToolHandler) {
	c.invoke(ctx, "runtime.tool.handler.runtime", func(ctx context.Context) (*tools.ToolCallResult, time.Duration, error) {
		start := time.Now()
		res, err := handler(ctx, c.sess, c.tc)
		return res, time.Since(start), err
	})
}

// invoke is the common pipeline shared by toolset tools and runtime-
// managed handlers: tracing, event emission, telemetry, error
// translation, and session message persistence. It is the only place
// where a tool actually runs.
func (c *call) invoke(ctx context.Context, spanName string, exec func(ctx context.Context) (*tools.ToolCallResult, time.Duration, error)) *tools.ToolCallResult {
	ctx, span := c.d.startSpan(ctx, spanName, trace.WithAttributes(
		attribute.String("tool.name", c.tc.Function.Name),
		attribute.String("agent", c.a.Name()),
		attribute.String("session.id", c.sess.ID),
		attribute.String("tool.call_id", c.tc.ID),
	))
	defer span.End()

	c.em.EmitToolCall(c.tc, c.tool, c.a.Name())

	res, duration, err := exec(ctx)
	telemetry.RecordToolCall(ctx, c.tc.Function.Name, c.sess.ID, c.a.Name(), duration, err)

	if err != nil {
		res = c.translateError(ctx, span, err)
	} else {
		span.SetStatus(codes.Ok, "tool handler completed")
		slog.DebugContext(ctx, "Tool call completed", "tool", c.tc.Function.Name, "output_length", len(res.Output))
	}

	// tool_response_transform fires here — BEFORE event emission, the
	// chat-message record, and the post_tool_use hook input — so any
	// rewrite (e.g. the redact_secrets builtin scrubbing tool output)
	// reaches every downstream consumer in one shot. The dispatch is
	// only paid when at least one hook is configured for the event, so
	// agents that haven't opted into output rewriting take the cheap
	// path through Dispatch's `exec.Has(event)` short-circuit.
	res.Output = c.applyToolResponseTransform(ctx, res.Output, false)

	c.em.EmitToolCallResponse(c.tc.ID, c.tool, res, res.Output, c.a.Name())
	c.recordToolResponse(res)
	return res
}

// applyToolResponseTransform fires [hooks.EventToolResponseTransform]
// for the supplied tool response payload and returns either the
// hook-supplied rewrite or payload unchanged.
//
// isError is forwarded as [hooks.Input.ToolError] so handlers can tell
// a real result apart from a synthesised dispatcher error response
// (validation failure, user rejection, post_tool_use block) without
// having to look at the tool name.
//
// The dispatch happens before any state-mutating step — emission,
// record, post_tool_use — so a single rewrite covers the UI feed,
// the persisted session file, the input the post_tool_use hook sees,
// and the messages going to the next LLM call.
func (c *call) applyToolResponseTransform(ctx context.Context, payload string, isError bool) string {
	if c.d.Hooks == nil {
		return payload
	}
	in := NewPostToolHooksInput(c.sess, c.tc, &tools.ToolCallResult{Output: payload, IsError: isError})
	result := c.d.Hooks.Dispatch(ctx, c.a, hooks.EventToolResponseTransform, in)
	if result == nil || result.UpdatedToolResponse == nil {
		return payload
	}
	return *result.UpdatedToolResponse
}

// translateError converts a tool-handler error into a [tools.ToolCallResult]
// suitable for the conversation, while annotating the span. Context-cancel
// errors are reported as user cancellation (Ok status); everything else is
// recorded as an error.
func (c *call) translateError(ctx context.Context, span trace.Span, err error) *tools.ToolCallResult {
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		slog.DebugContext(ctx, "Tool handler canceled by context", "tool", c.tc.Function.Name, "agent", c.a.Name(), "session_id", c.sess.ID)
		span.SetStatus(codes.Ok, "tool handler canceled by user")
		return tools.ResultError("The tool call was canceled by the user.")
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, "tool handler error")
	slog.ErrorContext(ctx, "Error calling tool", "tool", c.tc.Function.Name, "error", err)
	return tools.ResultError(fmt.Sprintf("Error calling tool: %v", err))
}

// recordToolResponse builds the chat message for a successful (or
// error-translated) tool result and adds it to the session.
func (c *call) recordToolResponse(res *tools.ToolCallResult) {
	// Tool response content must not be empty for API compatibility.
	content := res.Output
	if strings.TrimSpace(content) == "" {
		content = "(no output)"
	}

	msg := chat.Message{
		Role:       chat.MessageRoleTool,
		Content:    content,
		ToolCallID: c.tc.ID,
		IsError:    res.IsError,
		CreatedAt:  time.Now().Format(time.RFC3339),
	}

	if len(res.Images) > 0 {
		msg.MultiContent = buildMultiContent(content, res.Images)
	}

	c.addMessage(&msg)
}

// buildMultiContent attaches inline images to a tool response as a
// MultiContent payload, following the data-URL convention expected by
// chat clients.
func buildMultiContent(text string, images []tools.MediaContent) []chat.MessagePart {
	parts := make([]chat.MessagePart, 0, 1+len(images))
	parts = append(parts, chat.MessagePart{Type: chat.MessagePartTypeText, Text: text})
	for _, img := range images {
		parts = append(parts, chat.MessagePart{
			Type: chat.MessagePartTypeImageURL,
			ImageURL: &chat.MessageImageURL{
				URL:    "data:" + img.MimeType + ";base64," + img.Data,
				Detail: chat.ImageURLDetailAuto,
			},
		})
	}
	return parts
}

// postHook fires the post-tool-use hook. SystemMessage emission is the
// [HookDispatcher]'s responsibility. A terminating verdict
// (decision="block" / continue=false / exit 2) is propagated via the
// (stop, message) return. The tool result is forwarded to the hook so
// post_tool_use handlers can inspect ToolResponse / ToolError.
func (c *call) postHook(ctx context.Context, res *tools.ToolCallResult) (stop bool, message string) {
	if c.d.Hooks == nil {
		return false, ""
	}
	result := c.d.Hooks.Dispatch(ctx, c.a, hooks.EventPostToolUse, NewPostToolHooksInput(c.sess, c.tc, res))
	if result == nil || result.Allowed {
		return false, ""
	}
	return true, result.Message
}

// errorResponse appends an error tool-response to the session and emits
// the corresponding events. Used by validation, rejection, hook-block,
// and cancellation paths.
//
// The synthesised message is run through tool_response_transform like
// every other tool response so a configured rewriter (e.g. redact_secrets
// scrubbing user-supplied rejection reasons or hook-supplied block
// messages that quote tool input) sees the same payload the runtime
// would otherwise emit and persist.
func (c *call) errorResponse(ctx context.Context, errorMsg string) {
	errorMsg = c.applyToolResponseTransform(ctx, errorMsg, true)
	c.em.EmitToolCallResponse(c.tc.ID, c.tool, tools.ResultError(errorMsg), errorMsg, c.a.Name())
	c.addMessage(&chat.Message{
		Role:       chat.MessageRoleTool,
		Content:    errorMsg,
		ToolCallID: c.tc.ID,
		IsError:    true,
		CreatedAt:  time.Now().Format(time.RFC3339),
	})
}

// addMessage records msg in the session and emits MessageAdded.
func (c *call) addMessage(msg *chat.Message) {
	agentMsg := session.NewAgentMessage(c.a.Name(), msg)
	c.sess.AddMessage(agentMsg)
	c.em.EmitMessageAdded(c.sess.ID, agentMsg, c.a.Name())
}

// startSpan wraps Tracer.Start; a nil tracer is a no-op so callers don't
// need a guard.
func (d *Dispatcher) startSpan(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	if d.Tracer == nil {
		return ctx, trace.SpanFromContext(ctx)
	}
	return d.Tracer.Start(ctx, name, opts...)
}
