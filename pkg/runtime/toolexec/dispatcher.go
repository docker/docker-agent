package toolexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/concurrent"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/permissions"
	"github.com/docker/docker-agent/pkg/safety"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/telemetry"
	"github.com/docker/docker-agent/pkg/telemetry/genai"
	"github.com/docker/docker-agent/pkg/tools"
)

// Verdicts and sources surfaced via [HookDispatcher.NotifyApprovalDecision].
// The strings are part of the on_tool_approval_decision hook contract and
// must stay stable.
const (
	ApprovalDecisionAllow    = "allow"
	ApprovalDecisionDeny     = "deny"
	ApprovalDecisionCanceled = "canceled"

	// ApprovalSourceYolo marks an auto-approve produced by the
	// Autonomous safety mode (historically the --yolo flag; the
	// string is kept for hook-contract stability).
	ApprovalSourceYolo                       = "yolo"
	ApprovalSourceSessionPermissionsAllow    = "session_permissions_allow"
	ApprovalSourceSessionPermissionsDeny     = "session_permissions_deny"
	ApprovalSourceTeamPermissionsAllow       = "team_permissions_allow"
	ApprovalSourceTeamPermissionsDeny        = "team_permissions_deny"
	ApprovalSourcePreToolUseHookAllow        = "pre_tool_use_hook_allow"
	ApprovalSourcePreToolUseHookDeny         = "pre_tool_use_hook_deny"
	ApprovalSourcePermissionRequestHookDeny  = "permission_request_hook_deny"
	ApprovalSourcePermissionRequestHookAllow = "permission_request_hook_allow"
	// ApprovalSourceReadOnlyHint marks the legacy default's
	// auto-approve of read-only-annotated tools (sessions that never
	// chose an explicit safety mode).
	ApprovalSourceReadOnlyHint = "readonly_hint"
	// ApprovalSourceModeBalanced / ModeRestricted / ModeStrict /
	// ModeLegacy identify verdicts produced by the (mode × label)
	// table. Autonomous allows keep the stable "yolo" source above.
	ApprovalSourceModeBalanced           = "mode_balanced"
	ApprovalSourceModeRestricted         = "mode_restricted"
	ApprovalSourceModeStrict             = "mode_strict"
	ApprovalSourceModeLegacy             = "mode_legacy"
	ApprovalSourceUserApproved           = "user_approved"
	ApprovalSourceUserApprovedBalanced   = "user_approved_balanced"
	ApprovalSourceUserApprovedAutonomous = "user_approved_autonomous"
	ApprovalSourceUserApprovedTool       = "user_approved_tool"
	ApprovalSourceUserRejected           = "user_rejected"
	ApprovalSourceContextCanceled        = "context_canceled"
	// ApprovalSourceNonInteractiveDeny is recorded when a tool call
	// reaches [call.askUser] in a non-interactive session (eval, MCP
	// serve, A2A adapter, …). With no human at the keyboard and no
	// Resume listener, the deterministic safe answer is Deny; without
	// this guard the dispatcher would block on the Resume channel
	// forever.
	ApprovalSourceNonInteractiveDeny = "non_interactive_deny"
)

// CallOutcome captures the verdicts of a single tool invocation as
// observed by the dispatcher.
//
// Canceled and StopRun are mutually exclusive in practice but signal
// different things to the caller: cancellation cancels sibling calls in
// the current batch while letting the run loop continue with tool error
// responses; StopRun also terminates the agent's run loop with a
// user-visible reason produced by a post_tool_use hook deny verdict.
type CallOutcome struct {
	Canceled    bool
	StopRun     bool
	StopMessage string
}

// Emitter receives the events the [Dispatcher] emits while processing a
// batch of tool calls. Runtimes typically implement this by sending typed
// events to their event channel.
//
// Implementations must be safe for concurrent use: the dispatcher runs
// independent tool calls from the same batch in parallel.
//
// The dispatcher emits the events below. Runtime-managed handlers
// (registered via [Dispatcher.Handlers]) emit any additional runtime-specific
// events directly via the channel they captured at registration time.
type Emitter interface {
	EmitToolCall(toolCall tools.ToolCall, tool tools.Tool, agentName string)
	EmitToolCallOutput(toolCallID string, tool tools.Tool, output, agentName string)
	EmitToolCallResponse(toolCallID string, tool tools.Tool, result *tools.ToolCallResult, output, agentName string)
	EmitToolCallConfirmation(toolCall tools.ToolCall, tool tools.Tool, agentName string, metadata map[string]string)
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
	// [hooks.EventOnUserInput] attributed to the supplied agent.
	NotifyUserInput(ctx context.Context, a *agent.Agent, sessionID, label string)

	// NotifyApprovalDecision is invoked once per tool call after the
	// approval pipeline (auto-allow, deny, user confirmation, ...) has
	// resolved a verdict. Implementations typically fire
	// [hooks.EventOnToolApprovalDecision] with decision, source, and
	// the classifier's safety label set to the supplied strings (see
	// ApprovalDecision* / ApprovalSource* constants and
	// [safety.Class]).
	NotifyApprovalDecision(ctx context.Context, sess *session.Session, a *agent.Agent, tc tools.ToolCall, decision, source, safetyLabel string)
}

// ToolHandler is the signature for runtime-managed tool handlers
// (e.g. transfer_task, handoff, change_model). The dispatcher wraps every
// handler in tracing/telemetry/event-emission, so handlers MUST NOT emit
// ToolCall/ToolCallResponse themselves. Handlers that need to emit other
// event types should be wired by the caller to capture the relevant
// channel via closure when registering the handler.
type ToolHandler func(ctx context.Context, sess *session.Session, tc tools.ToolCall, rt tools.Runtime) (*tools.ToolCallResult, error)

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
	ResumeTypeApprove ResumeType = "approve"
	// ResumeTypeApproveBalanced approves the pending call and flips
	// the session to [session.SafetyPolicyBalanced].
	ResumeTypeApproveBalanced ResumeType = "approve-balanced"
	// ResumeTypeApproveAutonomous approves the pending call and flips
	// the session to [session.SafetyPolicyAutonomous].
	ResumeTypeApproveAutonomous ResumeType = "approve-autonomous"
	ResumeTypeApproveTool       ResumeType = "approve-tool"
	ResumeTypeReject            ResumeType = "reject"
)

// Legacy resume verbs accepted from older callers and normalized by
// [NormalizeResumeType]. Never emitted by new code.
const (
	// Deprecated: use [ResumeTypeApproveAutonomous].
	ResumeTypeApproveSession ResumeType = "approve-session"
	// Deprecated: use [ResumeTypeApproveBalanced].
	ResumeTypeApproveSafe ResumeType = "approve-safe"
	// Deprecated: use [ResumeTypeApproveBalanced].
	ResumeTypeApproveSafer ResumeType = "approve-safer"
)

// NormalizeResumeType maps legacy resume verbs onto the current set:
// approve-session → approve-autonomous, approve-safe / approve-safer →
// approve-balanced (the cautious mapping — old "safer" also waved
// unknown calls through, balanced asks about them). Current values
// pass through unchanged.
func NormalizeResumeType(t ResumeType) ResumeType {
	switch t {
	case ResumeTypeApproveSession:
		return ResumeTypeApproveAutonomous
	case ResumeTypeApproveSafe, ResumeTypeApproveSafer:
		return ResumeTypeApproveBalanced
	default:
		return t
	}
}

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

	// Recall enqueues a tool-produced steering message. Tool handlers reach it
	// through their [tools.Runtime] handle and may call it after the handler
	// has returned. When nil, [tools.Runtime.Recall] reports
	// [tools.ErrRecallNotSupported].
	Recall func(ctx context.Context, sess *session.Session, a *agent.Agent, message string) error

	confirmationMu sync.Mutex
}

var (
	errBatchCanceledByUser = errors.New("tool batch canceled by user")
	errBatchStoppedByHook  = errors.New("tool batch stopped by post_tool_use hook")
)

// Process runs every tool call in calls, emitting events through em. Calls in
// the same model batch are independent and execute in parallel; interactive
// confirmations are still serialized because resume decisions are not keyed by
// tool-call ID.
//
// Returns (stopRun, message) when a post_tool_use hook signalled a
// terminating verdict during this batch; the run loop then fans out the
// standard Error / notification / on_error stanzas before exiting.
// (false, "") in every other path — including user cancellation, which
// halts the *batch* but keeps the loop alive so the tool error responses can
// be sent back to the model on the next turn.
func (d *Dispatcher) Process(ctx context.Context, sess *session.Session, calls []tools.ToolCall, agentTools []tools.Tool, em Emitter) (stopRun bool, stopMessage string) {
	a := d.AgentFor(sess)
	slog.DebugContext(ctx, "Processing tool calls", "agent", a.Name(), "call_count", len(calls))

	toolByName := make(map[string]tools.Tool, len(agentTools))
	for _, t := range agentTools {
		toolByName[t.Name] = t
	}

	batchCtx, cancelBatch := context.WithCancelCause(ctx)
	defer cancelBatch(nil)

	var stopOnce sync.Once
	outcomes := concurrent.MapSlice(calls, func(tc tools.ToolCall) CallOutcome {
		c := d.newCall(sess, em, a, tc, toolByName)
		outcome := c.run(batchCtx)
		switch {
		case outcome.Canceled:
			stopOnce.Do(func() { cancelBatch(errBatchCanceledByUser) })
		case outcome.StopRun:
			stopOnce.Do(func() { cancelBatch(errBatchStoppedByHook) })
		}
		return outcome
	})

	for _, outcome := range outcomes {
		if outcome.StopRun {
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

	// pre_tool_use preempt-yolo lane result cache. The first
	// consultPreToolUsePreYolo call dispatches EventPreToolUsePreYolo
	// and stores the verdict; subsequent calls return the cached
	// result. Without the cache, approveAndRun + askUser +
	// confirmationMetadata would dispatch the lane up to three times
	// per tool call.
	preYoloComputed bool
	preYoloResult   *hooks.Result

	// Safety-label cache: the classifier result is stable for the
	// call, and permissionDecision + confirmationMetadata +
	// notifyApproval all consume it.
	labelComputed bool
	label         safety.Label
}

// safetyLabel labels the call for the (mode × label) table: shell
// commands via the pattern classifier, everything else via MCP
// annotation hints. Cached after the first call.
func (c *call) safetyLabel() safety.Label {
	if c.labelComputed {
		return c.label
	}
	c.labelComputed = true
	destructive := c.tool.Annotations.DestructiveHint != nil && *c.tool.Annotations.DestructiveHint
	c.label = safety.LabelToolCall(
		c.tc.Function.Name,
		ParseToolInput(c.tc.Function.Arguments),
		c.tool.Annotations.ReadOnlyHint,
		destructive,
	)
	return c.label
}

// run processes a single tool call and returns its outcome. All span
// and approval bookkeeping lives here so the call lifecycle is visible
// at a glance.
func (c *call) run(ctx context.Context) CallOutcome {
	// gen_ai.* attributes are always emitted (spec-compliant). Legacy
	// attribute names are added only when the OTel stability flag is
	// at its default — `OTEL_SEMCONV_STABILITY_OPT_IN=gen_ai_latest_experimental`
	// drops the legacy keys. Tool type is "function" because every tool
	// presented here is an LLM-callable function (transfer_task /
	// handoff are runtime-managed but still appear as functions to the
	// model).
	attrs := []attribute.KeyValue{
		attribute.String(genai.AttrOperationName, genai.OperationExecuteTool),
		attribute.String(genai.AttrToolName, c.tc.Function.Name),
		attribute.String(genai.AttrToolType, "function"),
		attribute.String(genai.AttrToolCallID, c.tc.ID),
		attribute.String(genai.AttrAgentNameRuntime, c.a.Name()),
		attribute.String(genai.AttrConversationID, c.sess.ID),
	}
	attrs = append(attrs, genai.LegacyToolAttributes(
		c.tc.Function.Name, string(c.tc.Type), c.a.Name(), c.sess.ID, c.tc.ID,
	)...)
	ctx, span := c.d.startSpan(ctx, "runtime.tool.call", trace.WithAttributes(attrs...))
	defer span.End()

	slog.DebugContext(ctx, "Processing tool call", "agent", c.a.Name(), "tool", c.tc.Function.Name, "session_id", c.sess.ID)

	if ctx.Err() != nil {
		msg := c.cancellationMessage(ctx)
		c.errorResponse(ctx, msg)
		span.SetStatus(codes.Ok, msg)
		return c.cancellationOutcome(ctx)
	}

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
//  0. pre_tool_use entries with preempt_yolo:true — user-authored
//     security hooks that fire BEFORE the deterministic pipeline so a
//     Deny or Ask verdict here cannot be bypassed by any safety mode
//     (including Autonomous) or permission allow rules. Allow /
//     no-opinion fall through.
//  1. [Decide] — custom Deny/Allow/Ask rules win outright; otherwise
//     the (safety mode × safety label) table produces the verdict.
//     An explicit ask rule goes straight to the user.
//  2. pre_tool_use hooks (LLM-judge, shell scripts, ...) — the
//     default lane, consulted ONLY when the mode said Ask. The hook
//     can Deny (block), Allow (skip the user prompt) or Ask (force
//     the prompt). Hooks may also rewrite tool arguments via
//     UpdatedInput, in which case the rewrite is applied here so the
//     user prompt and the tool handler both see the modified call.
//  3. legacy read-only auto-approve — sessions that never chose a
//     safety mode keep the pre-modes contract: read-only-annotated
//     tools run without prompting. Placed after the hook chain so an
//     LLM judge still gets a turn on those calls.
//  4. user confirmation — fallback prompt.
func (c *call) approveAndRun(ctx context.Context, runTool func() CallOutcome) CallOutcome {
	// Stage 0: pre_tool_use entries flagged with preempt_yolo:true.
	if r := c.consultPreToolUsePreYolo(ctx); r != nil {
		switch r.Decision {
		case hooks.DecisionDeny:
			slog.DebugContext(ctx, "Tool denied by preempt-yolo pre_tool_use hook", "tool", c.tc.Function.Name, "session_id", c.sess.ID, "reason", r.DecisionReason)
			c.notifyApproval(ctx, ApprovalDecisionDeny, ApprovalSourcePreToolUseHookDeny)
			rejectMsg := "The tool call was rejected by a pre_tool_use hook."
			if reason := strings.TrimSpace(r.DecisionReason); reason != "" {
				rejectMsg += " Reason: " + reason
			}
			c.errorResponse(ctx, rejectMsg)
			return CallOutcome{}
		case hooks.DecisionAsk:
			// A session-scoped allow grant (the interactive "T = always
			// allow this tool" decision, stored in sess.Permissions) is an
			// informed opt-in the user made in response to this very safety
			// prompt. Honor it instead of asking again, otherwise "always
			// allow" would re-prompt on every matching call. The safety
			// mode and the team/config permission layer stay subordinate
			// to the preempt-yolo verdict — see [call.sessionPermissionsAllow].
			if c.sessionPermissionsAllow() {
				slog.DebugContext(ctx, "preempt-yolo Ask overridden by session permission allow", "tool", c.tc.Function.Name, "session_id", c.sess.ID)
				c.notifyApproval(ctx, ApprovalDecisionAllow, ApprovalSourceSessionPermissionsAllow)
				return runTool()
			}
			return c.askUser(ctx, runTool)
		}
		// DecisionAllow / "" → advisory; fall through to Decide().
	}

	// Stage 1: custom rules + (mode × label) table.
	decision := c.permissionDecision()

	switch decision.Outcome {
	case OutcomeAllow:
		c.logAllow(decision)
		c.notifyApproval(ctx, ApprovalDecisionAllow, allowSourceForDecision(decision))
		return runTool()
	case OutcomeDeny:
		slog.DebugContext(ctx, "Tool denied", "tool", c.tc.Function.Name, "source", decision.Source, "session_id", c.sess.ID)
		c.notifyApproval(ctx, ApprovalDecisionDeny, denySourceForDecision(decision))
		c.errorResponse(ctx, denyErrorMessage(decision, c.tc.Function.Name))
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

	// Stage 2: the mode said Ask — consult the pre_tool_use hook chain.
	if outcome, handled := c.consultPreToolUseHook(ctx, runTool); handled {
		return outcome
	}

	// Stage 3: legacy default only — read-only tools never prompted
	// before safety modes existed, and sessions that never chose a
	// mode keep that contract. Explicit Strict deliberately does not
	// take this path: the user asked to be prompted for everything.
	if legacyReadOnlyAutoApprove(c.sess.GetSafetyPolicy(), c.safetyLabel()) {
		c.notifyApproval(ctx, ApprovalDecisionAllow, ApprovalSourceReadOnlyHint)
		return runTool()
	}
	return c.askUser(ctx, runTool)
}

func (c *call) permissionDecision() PermissionDecision {
	var checkers []NamedChecker
	if c.d.Permissions != nil {
		checkers = c.d.Permissions(c.sess)
	}
	return Decide(
		c.sess.GetSafetyPolicy(),
		c.safetyLabel(),
		checkers,
		c.tc.Function.Name,
		ParseToolInput(c.tc.Function.Arguments),
	)
}

func (c *call) autoApprovalAfterConfirmationWait() (PermissionDecision, bool) {
	if c.preYoloResult != nil && c.preYoloResult.Decision == hooks.DecisionAsk {
		// Even under a preempt-yolo Ask, a session-scoped allow grant that
		// landed while we were blocked on the resume channel (e.g. a
		// concurrent "always allow this tool" decision) is an informed
		// opt-in and takes effect. Mirrors approveAndRun's Stage 0: the
		// safety mode and the team/config layer stay subordinate.
		if c.sessionPermissionsAllow() {
			return PermissionDecision{Outcome: OutcomeAllow, Reason: ReasonChecker, Source: sessionPermissionsSource}, true
		}
		return PermissionDecision{}, false
	}
	// A mode escalation or allow grant may have landed while this call
	// was queued behind another confirmation. The legacy read-only
	// fast path is deliberately NOT rechecked here: it is static, so a
	// call that reached askUser despite it was explicitly asked about
	// (a hook or ask rule), and that prompt must be honored.
	decision := c.permissionDecision()
	return decision, decision.Outcome == OutcomeAllow
}

// sessionPermissionsAllow reports whether the session-scoped permission
// layer explicitly allows this call. That layer (sess.Permissions) is
// populated by the interactive "T = always allow this tool" grant and the
// session-permissions API/template — informed, session-scoped opt-ins —
// as opposed to the team/config layer (global settings + agent YAML) and
// the blanket --yolo, both of which the preempt-yolo lane must keep
// guarding.
//
// It is consulted when the preempt-yolo pre_tool_use lane wants to Ask so
// that a grant the user made in response to that safety prompt actually
// takes effect. Deny/Ask patterns in the same layer are honored via
// [permissions.Checker.CheckWithArgs] ordering (Deny > Allow > Ask), so a
// session-level Deny or explicit Ask never yields an allow here.
//
// For the shell tool a matching allow pattern is necessary but not
// sufficient: the generic matcher's trailing-* patterns are plain prefix
// matches, so the "T = always allow mkdir*" grant would also cover
// "mkdir x && rm -rf ~". Silencing a safety verdict demands the stricter
// word-boundary, no-metacharacter reading — see shellGrantCoversCommand.
func (c *call) sessionPermissionsAllow() bool {
	perms := c.sess.ClonePermissions()
	if perms == nil {
		return false
	}
	args := ParseToolInput(c.tc.Function.Arguments)
	checker := permissions.NewCheckerFromRules(perms.Allow, perms.Ask, perms.Deny)
	if checker.CheckWithArgs(c.tc.Function.Name, args) != permissions.Allow {
		return false
	}
	if c.tc.Function.Name == shellToolName {
		return shellGrantCoversCommand(perms.Allow, args)
	}
	return true
}

func (c *call) cancellationMessage(ctx context.Context) string {
	switch {
	case errors.Is(context.Cause(ctx), errBatchCanceledByUser):
		return "The tool call was canceled because another tool call in the same batch was canceled by the user."
	case errors.Is(context.Cause(ctx), errBatchStoppedByHook):
		return "The tool call was skipped because a post_tool_use hook signalled run termination."
	default:
		return "The tool call was canceled by the user."
	}
}

func (c *call) cancellationOutcome(ctx context.Context) CallOutcome {
	if errors.Is(context.Cause(ctx), errBatchStoppedByHook) {
		return CallOutcome{}
	}
	return CallOutcome{Canceled: true}
}

// consultPreToolUsePreYolo dispatches the preempt-yolo lane of the
// pre_tool_use hook chain for this call and caches the verdict. The
// preempt lane runs BEFORE the deterministic approval pipeline
// ([Decide], --yolo, permission patterns, the default pre_tool_use
// lane) so a deny/ask verdict here cannot be bypassed by
// auto-approval rules. Returns nil when no preempt-yolo entry is
// registered, the hooks dispatcher itself is nil, or the chain
// returned no opinion.
//
// Cached after the first call: approveAndRun consults it once,
// askUser reads its Decision to skip permission_request when the
// lane returned Ask, and confirmationMetadata reads its Metadata to
// enrich the confirmation event. Without the cache the chain would
// dispatch up to three times per call.
func (c *call) consultPreToolUsePreYolo(ctx context.Context) *hooks.Result {
	if c.preYoloComputed {
		return c.preYoloResult
	}
	c.preYoloComputed = true
	if c.d.Hooks == nil {
		return nil
	}
	c.preYoloResult = c.d.Hooks.Dispatch(ctx, c.a, hooks.EventPreToolUsePreYolo, NewHooksInput(c.sess, c.tc))
	return c.preYoloResult
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
	// The rewrite may change the shell command; drop the cached label
	// so the confirmation prompt classifies what will actually run.
	c.labelComputed = false
}

// notifyApproval forwards the resolved approval decision to the
// HookDispatcher, when one is configured. Also stamps the decision +
// source on the active runtime.tool.call span so denied / canceled
// calls are visible in trace dashboards (without it, denied tool calls
// are indistinguishable from user-canceled ones at the span level).
func (c *call) notifyApproval(ctx context.Context, decision, source string) {
	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		span.SetAttributes(
			attribute.String("cagent.approval.decision", decision),
			attribute.String("cagent.approval.source", source),
		)
	}
	if c.d.Hooks == nil {
		return
	}
	c.d.Hooks.NotifyApprovalDecision(ctx, c.sess, c.a, c.tc, decision, source, string(c.safetyLabel().Class))
}

// logAllow emits the auto-approval debug log appropriate to the reason
// that produced the [OutcomeAllow] decision.
func (c *call) logAllow(d PermissionDecision) {
	switch d.Reason {
	case ReasonMode:
		slog.Debug("Tool auto-approved by safety mode", "tool", c.tc.Function.Name, "source", d.Source, "session_id", c.sess.ID)
	case ReasonChecker:
		slog.Debug("Tool auto-approved by permissions", "tool", c.tc.Function.Name, "source", d.Source, "session_id", c.sess.ID)
	}
}

// allowSourceForDecision maps a [PermissionDecision] with [OutcomeAllow]
// onto the corresponding ApprovalSource* constant. Mode decisions
// already carry the constant in Source.
func allowSourceForDecision(d PermissionDecision) string {
	if d.Reason == ReasonMode {
		return d.Source
	}
	return allowSourceForChecker(d.Source)
}

// sessionPermissionsSource is the checker source label the runtime
// attaches to the session-scoped permission checker (see
// pkg/runtime.permissionCheckers). It distinguishes interactive /
// session-scoped grants from the team/config layer.
const sessionPermissionsSource = "session permissions"

// allowSourceForChecker maps a checker source label ("session permissions"
// or "permissions configuration") onto the corresponding ApprovalSource*
// allow constant.
func allowSourceForChecker(checkerSource string) string {
	if checkerSource == sessionPermissionsSource {
		return ApprovalSourceSessionPermissionsAllow
	}
	return ApprovalSourceTeamPermissionsAllow
}

// denySourceForChecker mirrors allowSourceForChecker for the deny path.
func denySourceForChecker(checkerSource string) string {
	if checkerSource == sessionPermissionsSource {
		return ApprovalSourceSessionPermissionsDeny
	}
	return ApprovalSourceTeamPermissionsDeny
}

// denySourceForDecision mirrors allowSourceForDecision for the deny
// path: mode decisions (only Restricted denies) already carry the
// ApprovalSource* constant in Source.
func denySourceForDecision(d PermissionDecision) string {
	if d.Reason == ReasonMode {
		return d.Source
	}
	return denySourceForChecker(d.Source)
}

// denyErrorMessage renders the tool-error text for an [OutcomeDeny]
// decision. Checker denials name the rule source; a mode denial (only
// the restricted mode denies) explains the fail-closed fallback so the
// model can adapt instead of blindly retrying.
func denyErrorMessage(d PermissionDecision, toolName string) string {
	if d.Reason == ReasonMode {
		return fmt.Sprintf("Tool '%s' is denied by the restricted safety mode: the call is not classified safe, and the mode's fallback denies instead of asking for confirmation.", toolName)
	}
	return fmt.Sprintf("Tool '%s' is denied by %s.", toolName, d.Source)
}

// askUser sends a confirmation event and waits for the user's response
// on the resume channel or for ctx cancellation. Only called when no
// permission rule auto-approved the tool.
//
// permission_request hooks fire first and may short-circuit the prompt
// with an explicit allow or deny verdict; returning nothing falls
// through to the interactive confirmation. The permission_request
// chain is SKIPPED entirely when the preempt-yolo lane of pre_tool_use
// produced an Ask verdict — that's the whole point of the lane
// preempting --yolo, and letting a policy-level permission_request
// hook auto-allow the call would unwind that protection.
func (c *call) askUser(ctx context.Context, runTool func() CallOutcome) CallOutcome {
	var hookMeta map[string]string
	if c.preYoloResult == nil || c.preYoloResult.Decision != hooks.DecisionAsk {
		outcome, handled, meta := c.runPermissionRequestHook(ctx, runTool)
		if handled {
			return outcome
		}
		hookMeta = meta
	}

	// Non-interactive sessions (eval, MCP serve, A2A adapter) have no
	// Resume listener. Blocking on the select below would hang forever.
	// The deterministic safe answer is Deny: nobody is at the keyboard
	// to approve, and an auto-Allow here would bypass whatever rule
	// routed the call to askUser in the first place (a checker
	// ForceAsk, a preempt-yolo Ask, or the default Ask).
	if c.sess.NonInteractive {
		slog.DebugContext(ctx, "Tool denied: non-interactive session reached askUser", "tool", c.tc.Function.Name, "session_id", c.sess.ID)
		c.notifyApproval(ctx, ApprovalDecisionDeny, ApprovalSourceNonInteractiveDeny)
		c.errorResponse(ctx, fmt.Sprintf("Tool '%s' requires user confirmation but the session is non-interactive.", c.tc.Function.Name))
		return CallOutcome{}
	}

	// ResumeRequest has no tool-call ID, so only one confirmation can be
	// visible and waiting on the shared channel at a time.
	c.d.confirmationMu.Lock()

	if ctx.Err() != nil {
		c.d.confirmationMu.Unlock()
		slog.DebugContext(ctx, "Context cancelled before confirmation", "tool", c.tc.Function.Name, "session_id", c.sess.ID)
		c.notifyApproval(ctx, ApprovalDecisionCanceled, ApprovalSourceContextCanceled)
		c.errorResponse(ctx, c.cancellationMessage(ctx))
		return c.cancellationOutcome(ctx)
	}

	if decision, ok := c.autoApprovalAfterConfirmationWait(); ok {
		c.d.confirmationMu.Unlock()
		c.logAllow(decision)
		c.notifyApproval(ctx, ApprovalDecisionAllow, allowSourceForDecision(decision))
		return runTool()
	}

	slog.DebugContext(ctx, "Tools not approved, waiting for resume", "tool", c.tc.Function.Name, "session_id", c.sess.ID)
	c.em.EmitToolCallConfirmation(c.tc, c.tool, c.a.Name(), c.confirmationMetadata(hookMeta))

	if c.d.Hooks != nil {
		c.d.Hooks.NotifyUserInput(ctx, c.a, c.sess.ID, "tool confirmation")
	}

	select {
	case req := <-c.d.Resume:
		c.d.confirmationMu.Unlock()
		return c.handleResume(ctx, req, runTool)
	case <-ctx.Done():
		c.d.confirmationMu.Unlock()
		slog.DebugContext(ctx, "Context cancelled while waiting for resume", "tool", c.tc.Function.Name, "session_id", c.sess.ID)
		c.notifyApproval(ctx, ApprovalDecisionCanceled, ApprovalSourceContextCanceled)
		c.errorResponse(ctx, c.cancellationMessage(ctx))
		return c.cancellationOutcome(ctx)
	}
}

// runPermissionRequestHook dispatches the permission_request hook just
// before the runtime would prompt the user for confirmation. The hook
// can short-circuit the prompt by returning permission_decision
// ("allow" or "deny") in hook_specific_output. A bare deny (Decision=
// "block" without permission_decision) is also honoured. Returning
// nothing keeps the existing behaviour and asks the user.
//
// When the hook does not short-circuit (handled=false) it may still
// have contributed key/value metadata for the confirmation prompt; that
// is returned so [call.askUser] can merge it onto the tool's own
// metadata.
func (c *call) runPermissionRequestHook(ctx context.Context, runTool func() CallOutcome) (outcome CallOutcome, handled bool, metadata map[string]string) {
	if c.d.Hooks == nil {
		return CallOutcome{}, false, nil
	}

	toolName := c.tc.Function.Name
	result := c.d.Hooks.Dispatch(ctx, c.a, hooks.EventPermissionRequest, &hooks.Input{
		SessionID:    c.sess.ID,
		ToolName:     toolName,
		ToolUseID:    c.tc.ID,
		ToolInput:    ParseToolInput(c.tc.Function.Arguments),
		SafetyPolicy: string(c.sess.GetSafetyPolicy()),
	})
	if result == nil {
		return CallOutcome{}, false, nil
	}

	if !result.Allowed {
		slog.DebugContext(ctx, "Tool denied by permission_request hook", "tool", toolName, "session_id", c.sess.ID, "reason", result.Message)
		// Stamp the deny on the runtime.tool.call span via notifyApproval
		// before returning. Without this the span would end with status
		// Ok and no cagent.approval.* attrs — denied-by-hook calls would
		// look identical to successful ones in trace dashboards, while
		// pre_tool_use deny does emit the attrs. Symmetry matters.
		c.notifyApproval(ctx, ApprovalDecisionDeny, ApprovalSourcePermissionRequestHookDeny)
		rejectMsg := "The tool call was rejected by a permission_request hook."
		if reason := strings.TrimSpace(result.Message); reason != "" {
			rejectMsg += " Reason: " + reason
		}
		c.errorResponse(ctx, rejectMsg)
		return CallOutcome{}, true, nil
	}

	if result.PermissionAllowed {
		slog.DebugContext(ctx, "Tool auto-approved by permission_request hook", "tool", toolName, "session_id", c.sess.ID, "reason", result.AdditionalContext)
		c.notifyApproval(ctx, ApprovalDecisionAllow, ApprovalSourcePermissionRequestHookAllow)
		return runTool(), true, nil
	}

	return CallOutcome{}, false, result.Metadata
}

// confirmationMetadata merges the tool's static metadata (set by the
// toolset) with the per-call metadata contributed by permission_request,
// the runtime's own safety label, and the preempt-yolo lane of
// pre_tool_use. Merge order — and therefore key-clash precedence — is:
//
//	tool static  <  permission_request  <  safety label  <  pre_tool_use (preempt_yolo)
//
// The runtime's classification (safety_label, blast_radius, category,
// reason) outranks a policy-level permission_request hook so the
// prompt's security badges can't be silently rewritten; a user-authored
// preempt hook outranks the runtime because it carries a more specific,
// security-critical verdict for this call.
func (c *call) confirmationMetadata(permissionMeta map[string]string) map[string]string {
	var preemptMeta map[string]string
	if c.preYoloResult != nil {
		preemptMeta = c.preYoloResult.Metadata
	}
	labelMeta := c.safetyLabel().Metadata()
	merged := make(map[string]string, len(c.tool.Metadata)+len(permissionMeta)+len(labelMeta)+len(preemptMeta))
	maps.Copy(merged, c.tool.Metadata)
	maps.Copy(merged, permissionMeta)
	maps.Copy(merged, labelMeta)
	maps.Copy(merged, preemptMeta)
	return merged
}

// handleResume applies the user's confirmation decision: run the tool
// (with optional session/tool-wide approval persistence) or emit a
// rejection error response.
func (c *call) handleResume(ctx context.Context, req ResumeRequest, runTool func() CallOutcome) CallOutcome {
	switch NormalizeResumeType(req.Type) {
	case ResumeTypeApprove:
		slog.DebugContext(ctx, "Resume signal received, approving tool", "tool", c.tc.Function.Name, "session_id", c.sess.ID)
		c.notifyApproval(ctx, ApprovalDecisionAllow, ApprovalSourceUserApproved)
		return runTool()
	case ResumeTypeApproveBalanced:
		slog.DebugContext(ctx, "Resume signal received, opting into balanced", "tool", c.tc.Function.Name, "session_id", c.sess.ID)
		c.sess.SetSafetyPolicy(session.SafetyPolicyBalanced)
		c.notifyApproval(ctx, ApprovalDecisionAllow, ApprovalSourceUserApprovedBalanced)
		return runTool()
	case ResumeTypeApproveAutonomous:
		slog.DebugContext(ctx, "Resume signal received, opting into autonomous", "tool", c.tc.Function.Name, "session_id", c.sess.ID)
		c.sess.SetSafetyPolicy(session.SafetyPolicyAutonomous)
		c.notifyApproval(ctx, ApprovalDecisionAllow, ApprovalSourceUserApprovedAutonomous)
		return runTool()
	case ResumeTypeApproveTool:
		approvedTool := req.ToolName
		if approvedTool == "" {
			approvedTool = c.tc.Function.Name
		}
		c.sess.AppendPermissionAllow(approvedTool)
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
		res, err := c.tool.Handler(ctx, c.tc, callRuntime{c})
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
		res, err := handler(ctx, c.sess, c.tc, callRuntime{c})
		return res, time.Since(start), err
	})
}

// callRuntime is the [tools.Runtime] handed to tool handlers. It carries only
// per-call state (no context); every method takes ctx from the caller, so
// handles held by background work stay valid after the tool call returns.
type callRuntime struct {
	c *call
}

func (r callRuntime) EmitOutput(ctx context.Context, output string) {
	output = r.c.applyToolResponseTransform(ctx, output, false)
	r.c.em.EmitToolCallOutput(r.c.tc.ID, r.c.tool, output, r.c.a.Name())
}

func (r callRuntime) Recall(ctx context.Context, message string) error {
	if r.c.d.Recall == nil {
		return tools.ErrRecallNotSupported
	}
	return r.c.d.Recall(ctx, r.c.sess, r.c.a, message)
}

func (r callRuntime) Supports(capability tools.Capability) bool {
	switch capability {
	case tools.CapabilityOutput:
		return true
	case tools.CapabilityRecall:
		return r.c.d.Recall != nil
	}
	return false
}

// invoke is the common pipeline shared by toolset tools and runtime-
// managed handlers: tracing, event emission, telemetry, error
// translation, and session message persistence. It is the only place
// where a tool actually runs.
func (c *call) invoke(ctx context.Context, spanName string, exec func(ctx context.Context) (*tools.ToolCallResult, time.Duration, error)) *tools.ToolCallResult {
	attrs := []attribute.KeyValue{
		attribute.String(genai.AttrOperationName, genai.OperationExecuteTool),
		attribute.String(genai.AttrToolName, c.tc.Function.Name),
		attribute.String(genai.AttrToolType, "function"),
		attribute.String(genai.AttrToolCallID, c.tc.ID),
		attribute.String(genai.AttrAgentNameRuntime, c.a.Name()),
		attribute.String(genai.AttrConversationID, c.sess.ID),
	}
	attrs = append(attrs, genai.LegacyToolAttributes(
		c.tc.Function.Name, string(c.tc.Type), c.a.Name(), c.sess.ID, c.tc.ID,
	)...)
	ctx, span := c.d.startSpan(ctx, spanName, trace.WithAttributes(attrs...))
	defer span.End()

	// gen_ai.tool.call.arguments capture is gated on the same opt-in as
	// chat content (`OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT`)
	// because tool arguments commonly carry the same PII / secrets as the
	// chat history that produced them (file paths, API tokens, prompts).
	if genai.IsContentCaptureEnabled() && c.tc.Function.Arguments != "" {
		span.SetAttributes(attribute.String(genai.AttrToolCallArguments, c.tc.Function.Arguments))
	}

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

	// gen_ai.tool.call.result captures the post-transform output so the
	// span matches what the LLM actually saw on the next turn (any
	// redact_secrets / scrubber rewrite is reflected). Same content-capture
	// gating as arguments above.
	if genai.IsContentCaptureEnabled() && res != nil && res.Output != "" {
		span.SetAttributes(attribute.String(genai.AttrToolCallResult, res.Output))
	}

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
	in.ToolCategory = c.tool.Category
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
		msg := c.cancellationMessage(ctx)
		slog.DebugContext(ctx, "Tool handler canceled by context", "tool", c.tc.Function.Name, "agent", c.a.Name(), "session_id", c.sess.ID)
		span.SetStatus(codes.Ok, msg)
		return tools.ResultError(msg)
	}
	if errors.Is(err, tools.ErrCallTimeout) {
		// The call site (mcp.Toolset.callTool) already logged the WARN and
		// span event for the timeout itself; avoid a duplicate RecordError.
		span.SetStatus(codes.Error, "tool call timed out")
		return tools.ResultError(err.Error())
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

	if len(res.Images) > 0 || len(res.Documents) > 0 {
		msg.MultiContent = chat.BuildToolResultMultiContent(content, res.Images, res.Documents)
	}

	c.addMessage(&msg)
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
