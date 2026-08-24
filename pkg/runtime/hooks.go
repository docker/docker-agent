package runtime

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
	"github.com/docker/docker-agent/pkg/runtime/toolexec"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/userconfig"
)

// buildHooksExecutors builds a [hooks.Executor] for every agent in the
// team. Every agent receives the always-on limit_large_tool_results
// tool_response_transform hook; user-configured hooks, agent-flag builtins,
// [builtins.AutoInjector]s, and response-cache hooks are layered onto that
// base configuration.
//
// Called once from [NewLocalRuntime] after r.workingDir, r.env and
// r.hooksRegistry are finalized; the resulting map is read-only for
// the lifetime of the runtime, so per-dispatch lookups don't need to
// lock.
func (r *LocalRuntime) buildHooksExecutors() {
	r.hooksExecByAgent = make(map[string]*hooks.Executor)
	for _, name := range r.team.AgentNames() {
		a, err := r.team.Agent(name)
		if err != nil {
			continue
		}
		cfg := builtins.ApplyAgentDefaults(a.Hooks(), builtins.AgentDefaults{
			AddDate:            a.AddDate(),
			AddEnvironmentInfo: a.AddEnvironmentInfo(),
			AddPromptFiles:     a.AddPromptFiles(),
			RedactSecrets:      a.RedactSecrets(),
		})
		cfg = applyAutoInjectors(cfg, r.autoInjectors)
		cfg = applyCacheDefault(cfg, a)
		if cfg == nil {
			continue
		}
		builtins.WarnIfSaferShellConfigured(cfg)
		r.hooksExecByAgent[name] = hooks.NewExecutorWithRegistry(cfg, r.workingDir, r.env, r.hooksRegistry)
	}
}

// applyAutoInjectors runs each AutoInjector against cfg, allocating a
// fresh Config when needed so a previously-empty agent picks up the
// injector's hooks. Returns nil iff cfg ends up empty after every
// injector has run, mirroring [ApplyAgentDefaults].
func applyAutoInjectors(cfg *hooks.Config, injectors []builtins.AutoInjector) *hooks.Config {
	if len(injectors) == 0 {
		return cfg
	}
	if cfg == nil {
		cfg = &hooks.Config{}
	}
	for _, inj := range injectors {
		if inj != nil {
			inj.AutoInject(cfg)
		}
	}
	if cfg.IsEmpty() {
		return nil
	}
	return cfg
}

// hooksExec returns the pre-built [hooks.Executor] for a, or nil when
// the agent has no hooks (see [buildHooksExecutors]).
func (r *LocalRuntime) hooksExec(a *agent.Agent) *hooks.Executor {
	if a == nil {
		return nil
	}
	return r.hooksExecByAgent[a.Name()]
}

// dispatchHook is the common dispatch path shared by every hook
// callsite: resolve the pre-built executor, dispatch, and emit any
// [Result.SystemMessage] as a Warning event. Errors are logged at warn
// level and surfaced as nil results so callers can use a single nil
// check to mean "nothing useful came back" — covering the
// not-configured, no-agent, and dispatch-failed cases uniformly.
//
// events may be nil for fire-and-forget callsites (notification,
// on_error, on_max_iterations, ...) where there's no Warning channel
// to emit on; the SystemMessage is then dropped by design rather than
// silently logged, since those events are themselves the user-facing
// notification mechanism.
func (r *LocalRuntime) dispatchHook(
	ctx context.Context,
	a *agent.Agent,
	event hooks.EventType,
	input *hooks.Input,
	events EventSink,
) *hooks.Result {
	exec := r.hooksExec(a)
	if exec == nil || !exec.Has(event) {
		return nil
	}

	// Auto-fill AgentName for events whose Input builder omits it (tool
	// events via toolexec.NewHooksInput), mirroring Cwd auto-fill in
	// Executor.Dispatch. Caller-set values win.
	if input != nil && input.AgentName == "" {
		input.AgentName = a.Name()
	}

	started := time.Now()
	if events != nil {
		events.Emit(HookStarted(event, input.SessionID, a.Name()))
	}
	result, err := exec.Dispatch(ctx, event, input)
	if events != nil {
		events.Emit(HookFinished(event, input.SessionID, result, err, time.Since(started), a.Name()))
	}
	if err != nil {
		slog.WarnContext(ctx, "Hook execution failed", "event", event, "agent", a.Name(), "error", err)
		return nil
	}

	if events != nil && result.SystemMessage != "" {
		events.Emit(Warning(result.SystemMessage, a.Name()))
	}
	return result
}

// executeSessionStartHooks fires session_start once at the top of RunStream.
// Its context is fed into the cache-stable instruction snapshot rather than
// appended to the visible transcript.
func (r *LocalRuntime) executeSessionStartHooks(ctx context.Context, sess *session.Session, a *agent.Agent, events EventSink) instructionObservation {
	return observeInstructions(r.dispatchHook(ctx, a, hooks.EventSessionStart, &hooks.Input{
		SessionID: sess.ID,
		Source:    "startup",
	}, events))
}

// executeTurnStartHooks fires turn_start before each model call. Recomputing
// its context each turn lets the instruction layer detect date and prompt-file
// changes without mutating the initial system prefix.
func (r *LocalRuntime) executeTurnStartHooks(ctx context.Context, sess *session.Session, a *agent.Agent, events EventSink) instructionObservation {
	return observeInstructions(r.dispatchHook(ctx, a, hooks.EventTurnStart, &hooks.Input{
		SessionID: sess.ID,
	}, events))
}

// Reason values reported in [hooks.Input.Reason] when [hooks.EventTurnEnd]
// fires. The runtime guarantees that turn_end runs once per turn that
// fired turn_start, no matter how the turn exited; the reason classifies
// which exit path the runtime took.
const (
	// turnEndReasonNormal — the model finished the turn cleanly and the
	// run loop is about to break out (no further iterations).
	turnEndReasonNormal = "normal"
	// turnEndReasonContinue — the turn finished cleanly and the loop is
	// about to start a new iteration (e.g. after tool calls, or after a
	// stop with a queued follow-up).
	turnEndReasonContinue = "continue"
	// turnEndReasonSteered — the turn finished and was followed by
	// drained steered messages, prompting a new iteration.
	turnEndReasonSteered = "steered"
	// turnEndReasonError — the model call failed and the runtime is
	// shutting down the run (handleStreamError returned non-retry).
	turnEndReasonError = "error"
	// turnEndReasonCanceled — the turn ended because the stream context
	// was cancelled (e.g. user Ctrl+C). Includes deferred firing on
	// any return path while ctx is done.
	turnEndReasonCanceled = "canceled"
	// turnEndReasonHookBlocked — a hook (before_llm_call or
	// post_tool_use) signalled run termination via a deny verdict.
	turnEndReasonHookBlocked = "hook_blocked"
	// turnEndReasonLoopDetected — the consecutive-tool-call loop
	// detector terminated the turn.
	turnEndReasonLoopDetected = "loop_detected"
	// turnEndReasonForceHandoff — the turn finished cleanly and the
	// runtime routed the conversation to the agent's configured
	// force_handoff target, prompting a new iteration.
	turnEndReasonForceHandoff = "force_handoff"
	// turnEndReasonBudgetExceeded — the run crossed a configured
	// budget ceiling (max_cost / max_tokens / max_time) and was
	// stopped. Distinct from "normal" so a budget-killed run is
	// distinguishable from a completed one in downstream analytics.
	turnEndReasonBudgetExceeded = "budget_exceeded"
)

// executeTurnEndHooks fires turn_end once per turn — symmetric to
// turn_start. Observational; the result is discarded. Reason is one
// of the turnEndReason* constants above and is reported via
// [hooks.Input.Reason] so handlers can branch on the exit path.
func (r *LocalRuntime) executeTurnEndHooks(ctx context.Context, sess *session.Session, a *agent.Agent, reason string, events EventSink) {
	r.dispatchHook(ctx, a, hooks.EventTurnEnd, &hooks.Input{
		SessionID: sess.ID,
		AgentName: a.Name(),
		Reason:    reason,
	}, events)
}

// contextMessages converts a context-providing hook's AdditionalContext
// into a one-element transient system-message slice ready to thread
// through [session.Session.GetMessages]. Returns nil for empty results
// so callers can pass it straight to [slices.Concat] without a guard.
func contextMessages(result *hooks.Result) []chat.Message {
	if result == nil || result.AdditionalContext == "" {
		return nil
	}
	return []chat.Message{{
		Role:    chat.MessageRoleSystem,
		Content: result.AdditionalContext,
	}}
}

type instructionObservation struct {
	messages []chat.Message
	sources  []session.InstructionSource
}

func (o instructionObservation) legacyMessages() []chat.Message {
	messages := slices.Clone(o.messages)
	for _, source := range o.sources {
		if !source.Available || source.Removed || source.SetMarker || strings.TrimSpace(source.Content) == "" {
			continue
		}
		messages = append(messages, chat.Message{Role: chat.MessageRoleSystem, Content: source.Content})
	}
	return messages
}

func observeInstructions(result *hooks.Result) instructionObservation {
	observation := instructionObservation{messages: contextMessages(result)}
	if result == nil {
		return observation
	}
	for _, source := range result.InstructionContext {
		observation.sources = append(observation.sources, session.InstructionSource{
			Key:            source.Key,
			Group:          source.Group,
			Label:          source.Label,
			Content:        source.Content,
			ChangedContent: source.ChangedContent,
			RemovedContent: source.RemovedContent,
			Removed:        source.Removed,
			Available:      !source.Unavailable,
			CompleteGroup:  source.CompleteGroup,
			SetMarker:      source.SetMarker,
		})
	}
	return observation
}

func instructionSources(sessionStart, userPrompt []chat.Message, turnStart instructionObservation, additional ...session.InstructionSource) []session.InstructionSource {
	sources := []session.InstructionSource{
		instructionSource("hooks/session-start", "environment context", sessionStart),
		instructionSource("hooks/user-prompt", "user-request context", userPrompt),
		instructionSource("hooks/turn-start", "dynamic context", turnStart.messages),
	}
	sources = append(sources, additional...)
	return append(sources, turnStart.sources...)
}

func (r *LocalRuntime) messagesWithDynamicContext(
	ctx context.Context,
	sess *session.Session,
	a *agent.Agent,
	sources []session.InstructionSource,
	legacyExtras []chat.Message,
) []chat.Message {
	if !userconfig.Get().CacheStablePromptsEnabled() {
		if sess.ClearInstructionContext() {
			if err := r.sessionStore.UpdateSession(ctx, sess); err != nil {
				slog.WarnContext(ctx, "Failed to clear instruction context", "session_id", sess.ID, "error", err)
			}
		}
		return sess.GetMessagesWithoutInstructionContext(a, legacyExtras...)
	}
	if sess.PrepareInstructionContext(sources) {
		if err := r.sessionStore.UpdateSession(ctx, sess); err != nil {
			slog.WarnContext(ctx, "Failed to persist instruction context", "session_id", sess.ID, "error", err)
		}
	}
	return sess.GetMessages(a)
}

func instructionSource(key, label string, messages []chat.Message) session.InstructionSource {
	contents := make([]string, 0, len(messages))
	for _, message := range messages {
		if strings.TrimSpace(message.Content) != "" {
			contents = append(contents, message.Content)
		}
	}
	content := strings.Join(contents, "\n")
	return session.InstructionSource{
		Key:       key,
		Label:     label,
		Content:   content,
		Removed:   content == "",
		Available: true,
	}
}

// executeSessionEndHooks fires session_end when the run loop exits.
func (r *LocalRuntime) executeSessionEndHooks(ctx context.Context, sess *session.Session, a *agent.Agent) {
	r.dispatchHook(ctx, a, hooks.EventSessionEnd, &hooks.Input{
		SessionID: sess.ID,
		Reason:    "stream_ended",
	}, nil)
}

// executeStopHooks fires stop hooks when the model finishes responding,
// passing the final response content as stop_response. SystemMessage is
// surfaced as a Warning by [dispatchHook]. AgentName + LastUserMessage
// are populated so builtins like cache_response can key on the user's
// question and resolve the agent through the runtime closure.
func (r *LocalRuntime) executeStopHooks(ctx context.Context, sess *session.Session, a *agent.Agent, responseContent string, events EventSink) {
	r.dispatchHook(ctx, a, hooks.EventStop, &hooks.Input{
		SessionID:       sess.ID,
		AgentName:       a.Name(),
		StopResponse:    responseContent,
		LastUserMessage: sess.GetLastUserMessageContent(),
	}, events)
}

// notifyError fires both notification(level=error) and on_error in one
// call. They're always emitted together (an error is always also a
// user-facing notification), so collapsing them into one call expresses
// intent more directly than firing two events at every callsite.
func (r *LocalRuntime) notifyError(ctx context.Context, a *agent.Agent, sessionID, message string) {
	r.notify(ctx, a, hooks.EventNotification, sessionID, "error", message)
	r.notify(ctx, a, hooks.EventOnError, sessionID, "error", message)
}

// notifyMaxIterations fires both notification(level=warning) and
// on_max_iterations. Same rationale as [notifyError]: the two are
// always emitted together when the iteration limit is reached.
func (r *LocalRuntime) notifyMaxIterations(ctx context.Context, a *agent.Agent, sessionID, message string) {
	r.notify(ctx, a, hooks.EventNotification, sessionID, "warning", message)
	r.notify(ctx, a, hooks.EventOnMaxIterations, sessionID, "warning", message)
}

// notifyBudgetExceeded fires notification(level=warning) when a run is
// stopped by its budget. It deliberately reuses the generic notification
// event rather than introducing a dedicated on_budget_exceeded: the
// budget_exceeded runtime event already carries the structured payload
// (limit, used, max) that an audit consumer wants, so a second hook
// surface would duplicate it for no gain. A dedicated event is easy to
// add later if hook authors ask to match on it specifically.
func (r *LocalRuntime) notifyBudgetExceeded(ctx context.Context, a *agent.Agent, sessionID, message string) {
	r.notify(ctx, a, hooks.EventNotification, sessionID, "warning", message)
}

// notify is the shared dispatch path for the (level, message)-shaped
// hook events: notification, on_error, on_max_iterations. They all
// take the same Input fields and are observational (no Result is
// honored), so a single helper covers them all.
func (r *LocalRuntime) notify(ctx context.Context, a *agent.Agent, event hooks.EventType, sessionID, level, message string) {
	r.dispatchHook(ctx, a, event, &hooks.Input{
		SessionID:           sessionID,
		NotificationLevel:   level,
		NotificationMessage: message,
	}, nil)
}

// Agent-switch kinds passed via [hooks.Input.AgentSwitchKind] to
// describe what kind of transition triggered the on_agent_switch
// event. Constants instead of literals so the hook contract is
// discoverable from the runtime side and a typo trips a compile
// error.
const (
	agentSwitchKindTransferTask       = "transfer_task"
	agentSwitchKindTransferTaskReturn = "transfer_task_return"
	agentSwitchKindHandoff            = "handoff"
	agentSwitchKindForceHandoff       = "force_handoff"
)

// executeOnAgentSwitchHooks fires on_agent_switch when the runtime
// changes the active agent. Observational; failures are logged. The
// hook runs alongside the existing [AgentSwitching] event, so users
// who already consume that event see no behaviour change.
//
// The previous agent's model-endpoint snapshot is built only when at
// least one hook is configured for this event so audit-free
// deployments don't pay the team-lookup + per-model allocation on
// every agent switch (matches the cheap-when-unused property of the
// other hook callsites).
func (r *LocalRuntime) executeOnAgentSwitchHooks(ctx context.Context, a *agent.Agent, sessionID, fromAgent, toAgent, kind string) {
	exec := r.hooksExec(a)
	if exec == nil || !exec.Has(hooks.EventOnAgentSwitch) {
		return
	}
	r.dispatchHook(ctx, a, hooks.EventOnAgentSwitch, &hooks.Input{
		SessionID:       sessionID,
		FromAgent:       fromAgent,
		ToAgent:         toAgent,
		AgentSwitchKind: kind,
		FromAgentModels: r.fromAgentModels(fromAgent),
	}, nil)
}

// fromAgentModels snapshots the previous agent's configured model
// endpoints into the wire-friendly [hooks.ModelEndpoint] form. Hooks
// that act on the previous agent's models (e.g. the stock `unload`
// builtin) read this slice instead of poking at the runtime, so the
// hook payload stays self-contained.
//
// Returns nil when there is no previous agent or the lookup fails so
// the JSON wire payload omits the field via `omitempty`.
func (r *LocalRuntime) fromAgentModels(fromAgent string) []hooks.ModelEndpoint {
	if fromAgent == "" {
		return nil
	}
	from, err := r.team.Agent(fromAgent)
	if err != nil {
		slog.Debug("on_agent_switch: from-agent lookup failed",
			"agent", fromAgent, "error", err)
		return nil
	}
	configured := from.ConfiguredModels()
	out := make([]hooks.ModelEndpoint, 0, len(configured))
	for _, p := range configured {
		cfg := p.BaseConfig()
		out = append(out, hooks.ModelEndpoint{
			Provider:  cfg.ModelConfig.Provider,
			Model:     cfg.ModelConfig.Model,
			BaseURL:   cfg.BaseURL,
			UnloadAPI: cfg.ModelConfig.UnloadAPI(),
		})
	}
	return out
}

// executeOnSessionResumeHooks fires on_session_resume when the user
// explicitly approves continuation past the configured
// max_iterations limit. Observational; failures are logged. The hook
// runs alongside the existing event-channel signalling so audit /
// quota / alerting pipelines can react without subscribing to the
// per-session channel.
func (r *LocalRuntime) executeOnSessionResumeHooks(ctx context.Context, a *agent.Agent, sessionID string, prevMax, newMax int) {
	r.dispatchHook(ctx, a, hooks.EventOnSessionResume, &hooks.Input{
		SessionID:             sessionID,
		PreviousMaxIterations: prevMax,
		NewMaxIterations:      newMax,
	}, nil)
}

// Verdicts and sources for [hooks.EventOnToolApprovalDecision]. Constants
// instead of literals so the contract between executeWithApproval and
// the hook protocol is discoverable from the runtime side and a typo
// trips a compile error.
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
	ApprovalSourceModeBalanced            = "mode_balanced"
	ApprovalSourceModeRestricted          = "mode_restricted"
	ApprovalSourceModeStrict              = "mode_strict"
	ApprovalSourceModeLegacy              = "mode_legacy"
	ApprovalSourceUserApproved            = "user_approved"
	ApprovalSourceUserApprovedBalanced    = "user_approved_balanced"
	ApprovalSourceUserApprovedAutonomous  = "user_approved_autonomous"
	ApprovalSourceUserApprovedTool        = "user_approved_tool"
	ApprovalSourceUserRejected            = "user_rejected"
	ApprovalSourceContextCanceled         = "context_canceled"
)

// executeOnToolApprovalDecisionHooks fires on_tool_approval_decision
// after the runtime's approval chain has resolved a verdict for a
// tool call. Fired once per call from each return path of
// [executeWithApproval], so a single hook gets one record per tool
// call regardless of which step decided.
func (r *LocalRuntime) executeOnToolApprovalDecisionHooks(
	ctx context.Context,
	sess *session.Session,
	a *agent.Agent,
	toolCall tools.ToolCall,
	decision, source, safetyLabel string,
) {
	input := toolexec.NewHooksInput(sess, toolCall)
	input.ApprovalDecision = decision
	input.ApprovalSource = source
	input.SafetyLabel = safetyLabel
	r.dispatchHook(ctx, a, hooks.EventOnToolApprovalDecision, input, nil)
}

// executeBeforeLLMCallHooks fires before_llm_call just before each
// model call. A terminating verdict (decision="block" / continue=false
// / exit 2) stops the run loop — see [hooks.EventBeforeLLMCall] for
// the contract. Hooks that just want to contribute system messages
// should target turn_start instead.
//
// modelID and iteration are surfaced verbatim via
// [hooks.Input.ModelID] / [hooks.Input.Iteration] so handlers (notably
// the max_iterations builtin) don't have to recompute them — the
// loop's resolved values reflect per-tool overrides and alloy-mode
// selection that an Agent.Model() lookup would miss.
//
// messages is the conversation snapshot the runtime is about to send
// to the model. Hooks may return a rewrite via
// [hooks.HookSpecificOutput.UpdatedMessages] (e.g. the redact_secrets
// builtin scrubbing outbound chat content); the rewrite is returned
// in the third tuple value when present, nil otherwise. Callers must
// swap the rewrite in BEFORE the model call so the LLM never sees the
// original content.
func (r *LocalRuntime) executeBeforeLLMCallHooks(
	ctx context.Context,
	sess *session.Session,
	a *agent.Agent,
	modelID string,
	iteration int,
	messages []chat.Message,
) (stop bool, message string, rewritten []chat.Message) {
	exec := r.hooksExec(a)
	if exec == nil || !exec.Has(hooks.EventBeforeLLMCall) {
		return false, "", nil
	}
	// Only carry the conversation snapshot when at least one hook is
	// actually wired — dispatchHook short-circuits before then, but the
	// JSON-encoded copy of `messages` would still be paid here.
	result := r.dispatchHook(ctx, a, hooks.EventBeforeLLMCall, &hooks.Input{
		SessionID: sess.ID,
		AgentName: a.Name(),
		ModelID:   modelID,
		Iteration: iteration,
		Messages:  messages,
	}, nil)
	if result == nil {
		return false, "", nil
	}
	if !result.Allowed {
		return true, result.Message, nil
	}
	return false, "", result.UpdatedMessages
}

// executeAfterLLMCallHooks fires after_llm_call after a successful
// model call, before the response is recorded into the session and
// tool calls are dispatched. The assistant text content is passed via
// stop_response (matching the stop event), so handlers can reuse the
// same parsing logic. The per-turn token usage and computed USD cost
// are forwarded via [hooks.Input.Usage] and [hooks.Input.Cost] so
// sidecar cost ledgers can record per-call spend from the payload
// alone. cost is a *float64 so an unpriced model (nil) is distinct on
// the wire from a priced free call (a pointer to 0); the caller owns
// that distinction. Failed model calls fire on_error instead and skip
// this event.
func (r *LocalRuntime) executeAfterLLMCallHooks(ctx context.Context, sess *session.Session, a *agent.Agent, modelID, responseContent string, usage *chat.Usage, cost *float64) {
	r.dispatchHook(ctx, a, hooks.EventAfterLLMCall, &hooks.Input{
		SessionID:       sess.ID,
		AgentName:       a.Name(),
		ModelID:         modelID,
		StopResponse:    responseContent,
		LastUserMessage: sess.GetLastUserMessageContent(),
		Usage:           usage,
		Cost:            cost,
	}, nil)
}

// executeOnUserInputHooks fires on_user_input when the runtime is about
// to wait for the user (tool confirmation, elicitation, max iterations,
// stream stopped). The agent is passed explicitly so callsites that
// already resolved the session's agent (pinned sessions included)
// attribute the event to that agent; paths without an agent handle
// (like the elicitation handler) fall back to CurrentAgent at the
// callsite. A nil agent is a no-op.
func (r *LocalRuntime) executeOnUserInputHooks(ctx context.Context, a *agent.Agent, sessionID, logContext string) {
	if a == nil {
		return
	}
	slog.DebugContext(ctx, "Executing on-user-input hooks", "context", logContext)
	r.dispatchHook(ctx, a, hooks.EventOnUserInput, &hooks.Input{
		SessionID: sessionID,
	}, nil)
}

// executeBeforeCompactionHooks fires before a session compaction. The
// hook may veto the compaction (Decision: "block") or supply a custom
// summary string via [hooks.HookSpecificOutput.Summary] to skip the
// LLM-based summarization. The Result is returned verbatim so the
// caller (doCompact) can act on Allowed and Summary.
//
// Returns nil when no hook is configured for this event so the caller
// can use a single nil check to mean "nothing to do, fall through to
// the default LLM-based path".
func (r *LocalRuntime) executeBeforeCompactionHooks(
	ctx context.Context,
	sess *session.Session,
	a *agent.Agent,
	reason string,
	contextLimit int64,
	events EventSink,
) *hooks.Result {
	inputTokens, outputTokens := sess.Usage()
	return r.dispatchHook(ctx, a, hooks.EventBeforeCompaction, &hooks.Input{
		SessionID:        sess.ID,
		InputTokens:      inputTokens,
		OutputTokens:     outputTokens,
		ContextLimit:     contextLimit,
		CompactionReason: reason,
	}, events)
}

// executeAfterCompactionHooks fires after a successful compaction has
// applied a summary to the session. Purely observational — the result
// is discarded.
//
// The Input carries the *pre-compaction* token counts (what was
// summarized), not the new ones, so handlers can naturally express
// "compacted from X to Y". The post-compaction counts are reflected
// in the next [NewTokenUsageEvent] the runtime emits.
func (r *LocalRuntime) executeAfterCompactionHooks(
	ctx context.Context,
	sess *session.Session,
	a *agent.Agent,
	reason string,
	contextLimit int64,
	preInputTokens, preOutputTokens int64,
	summary string,
	events EventSink,
) {
	r.dispatchHook(ctx, a, hooks.EventAfterCompaction, &hooks.Input{
		SessionID:        sess.ID,
		InputTokens:      preInputTokens,
		OutputTokens:     preOutputTokens,
		ContextLimit:     contextLimit,
		CompactionReason: reason,
		Summary:          summary,
	}, events)
}

// executeUserPromptSubmitHooks fires user_prompt_submit once per user
// message, after the prompt has been added to the session and before
// the first model call of the turn. A terminating verdict
// (decision="block" / continue=false / exit 2) stops the run loop;
// AdditionalContext is returned as a transient system message that
// callers splice into the conversation for that turn only.
func (r *LocalRuntime) executeUserPromptSubmitHooks(ctx context.Context, sess *session.Session, a *agent.Agent, prompt string, events EventSink) (stop bool, message string, contextMsgs []chat.Message) {
	result := r.dispatchHook(ctx, a, hooks.EventUserPromptSubmit, &hooks.Input{
		SessionID: sess.ID,
		Prompt:    prompt,
	}, events)
	if result == nil {
		return false, "", nil
	}
	if !result.Allowed {
		return true, result.Message, nil
	}
	return false, "", contextMessages(result)
}

// executeUserSteeringMessagesSubmitHooks fires
// user_steering_messages_submit each time the runtime drains the
// steering queue and appends the queued user messages to the session.
// It is the steering-queue analogue of user_prompt_submit: the drained
// messages are passed in SteeringMessages, a terminating verdict
// (decision="block" / continue=false / exit 2) stops the run loop, and
// AdditionalContext is returned as a transient system message that the
// caller threads into the steered turn only — never persisted.
func (r *LocalRuntime) executeUserSteeringMessagesSubmitHooks(ctx context.Context, sess *session.Session, a *agent.Agent, steeringMessages []string, events EventSink) (stop bool, message string, contextMsgs []chat.Message) {
	result := r.dispatchHook(ctx, a, hooks.EventUserSteeringMessagesSubmit, &hooks.Input{
		SessionID:        sess.ID,
		SteeringMessages: steeringMessages,
	}, events)
	if result == nil {
		return false, "", nil
	}
	if !result.Allowed {
		return true, result.Message, nil
	}
	return false, "", contextMessages(result)
}

// executeUserFollowupSubmitHooks fires user_followup_submit each time
// the runtime dequeues a follow-up message at the end of a turn and
// starts a fresh turn for it. Follow-ups are user messages queued for
// end-of-turn processing (the FollowUp API / queue), distinct from
// mid-turn steering. It mirrors user_prompt_submit: the follow-up text
// is passed in Prompt, a terminating verdict (decision="block" /
// continue=false / exit 2) stops the run loop, and AdditionalContext is
// returned as a transient system message that the caller threads into
// the follow-up turn only — never persisted.
func (r *LocalRuntime) executeUserFollowupSubmitHooks(ctx context.Context, sess *session.Session, a *agent.Agent, prompt string, events EventSink) (stop bool, message string, contextMsgs []chat.Message) {
	result := r.dispatchHook(ctx, a, hooks.EventUserFollowupSubmit, &hooks.Input{
		SessionID: sess.ID,
		Prompt:    prompt,
	}, events)
	if result == nil {
		return false, "", nil
	}
	if !result.Allowed {
		return true, result.Message, nil
	}
	return false, "", contextMessages(result)
}

// executePreCompactHooks fires pre_compact just before compaction.
// The trigger reason ("manual", "auto", "overflow", "tool_overflow")
// is reported in [hooks.Input.Source]. A terminating verdict skips
// compaction entirely; AdditionalContext is appended to the
// compaction prompt so handlers can steer the summary.
func (r *LocalRuntime) executePreCompactHooks(ctx context.Context, sess *session.Session, a *agent.Agent, source string, events EventSink) (skip bool, message, additionalPrompt string) {
	result := r.dispatchHook(ctx, a, hooks.EventPreCompact, &hooks.Input{
		SessionID: sess.ID,
		Source:    source,
	}, events)
	if result == nil {
		return false, "", ""
	}
	if !result.Allowed {
		return true, result.Message, ""
	}
	return false, "", result.AdditionalContext
}

// executeSubagentStopHooks fires subagent_stop when a sub-agent
// (transferred task, background agent, skill sub-session) finishes.
// It always runs against the *parent* agent's executor: subagent_stop
// is by design observed by whoever spawned the sub-agent, so handlers
// configured on the parent see every child completion in one place
// without having to be replicated on each child.
func (r *LocalRuntime) executeSubagentStopHooks(ctx context.Context, parent, child *session.Session, parentAgent *agent.Agent, subAgentName, response string) {
	r.dispatchHook(ctx, parentAgent, hooks.EventSubagentStop, &hooks.Input{
		SessionID:       child.ID,
		ParentSessionID: parent.ID,
		AgentName:       subAgentName,
		StopResponse:    response,
	}, nil)
}
