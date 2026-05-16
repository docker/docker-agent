// Package hooks provides lifecycle hooks for agent tool execution.
// Hooks allow users to run shell commands or in-process Go functions at
// various points during the agent's execution lifecycle, providing
// deterministic control over agent behavior.
package hooks

import (
	"encoding/json"

	"github.com/docker/docker-agent/pkg/chat"
)

// EventType identifies a hook event.
type EventType string

const (
	// EventPreToolUse fires before a tool call. Can allow/deny/modify it.
	EventPreToolUse EventType = "pre_tool_use"
	// EventPostToolUse fires after a tool completes — both success and
	// failure. The result is delivered in [Input.ToolResponse]; failed
	// calls carry an is_error flag and any error text. Returning
	// decision="block" (or continue=false / exit code 2) stops the run
	// loop after the current tool batch — useful for circuit-breaker
	// patterns like a tool-call loop detector.
	EventPostToolUse EventType = "post_tool_use"
	// EventPermissionRequest fires just before the runtime would prompt
	// the user to confirm a tool call (i.e. when neither --yolo nor a
	// permissions rule short-circuited the decision and the tool is not
	// read-only). The hook can short-circuit the prompt by returning
	// [HookSpecificOutput.PermissionDecision] = "allow" (sets
	// [Result.PermissionAllowed] true — the runtime invokes the tool
	// without asking) or "deny" (sets [Result.Allowed] false — the
	// runtime rejects the tool with the hook's reason). Returning
	// nothing falls through to the interactive confirmation.
	//
	// Unlike pre_tool_use — where allow is the implicit default and only
	// deny carries new information — here allow is the explicit
	// auto-approve verdict; that asymmetry is why permission_request
	// has its own [Result.PermissionAllowed] flag separate from
	// [Result.Allowed].
	EventPermissionRequest EventType = "permission_request"
	// EventSessionStart fires when a session begins or resumes.
	EventSessionStart EventType = "session_start"
	// EventUserPromptSubmit fires once per user prompt, after the user
	// has submitted their message and before the first model call of
	// the turn. Returning decision="block" (or continue=false / exit
	// code 2) stops the run loop before the model is invoked.
	// AdditionalContext is spliced into the conversation as a transient
	// system message for that turn only.
	EventUserPromptSubmit EventType = "user_prompt_submit"
	// EventTurnStart fires at the start of every agent turn (each model
	// call). AdditionalContext is injected transiently and never persisted.
	EventTurnStart EventType = "turn_start"
	// EventTurnEnd fires once per agent turn (each model call) when the
	// turn finishes — symmetric to [EventTurnStart]. It runs no matter
	// why the turn ended: a normal stop, an error, a hook-driven
	// shutdown, the loop detector, or context cancellation. The reason
	// is reported in [Input.Reason] using one of the turnEndReason*
	// constants in the runtime package ("normal", "continue",
	// "steered", "error", "canceled", "hook_blocked",
	// "loop_detected"). Observational; output is ignored.
	EventTurnEnd EventType = "turn_end"
	// EventBeforeLLMCall fires immediately before each model call.
	// Returning decision="block" (or continue=false / exit code 2)
	// stops the run loop before the model is invoked — useful for hard
	// budget guards. Use turn_start to contribute system messages;
	// this event's AdditionalContext is not consumed.
	EventBeforeLLMCall EventType = "before_llm_call"
	// EventAfterLLMCall fires immediately after a successful model call,
	// before the response is recorded. Failed calls fire EventOnError.
	EventAfterLLMCall EventType = "after_llm_call"
	// EventSessionEnd fires when a session terminates.
	EventSessionEnd EventType = "session_end"
	// EventPreCompact fires just before the runtime compacts the session
	// transcript. The trigger is reported in [Input.Source]: "manual",
	// "auto", "overflow", or "tool_overflow". Returning decision="block"
	// (or continue=false / exit code 2) cancels the compaction.
	// AdditionalContext is appended to the compaction prompt and lets
	// the hook steer the summary without modifying the agent's instruction.
	EventPreCompact EventType = "pre_compact"
	// EventSubagentStop fires when a sub-agent (transferred task,
	// background agent, skill sub-session) finishes. The sub-agent's
	// name is in [Input.AgentName] and its final assistant message in
	// [Input.StopResponse].
	EventSubagentStop EventType = "subagent_stop"
	// EventOnUserInput fires when the agent needs input from the user.
	EventOnUserInput EventType = "on_user_input"
	// EventStop fires when the model finishes its response.
	EventStop EventType = "stop"
	// EventNotification fires when the agent emits a notification.
	EventNotification EventType = "notification"
	// EventOnError fires when the runtime hits an error during a turn.
	EventOnError EventType = "on_error"
	// EventOnMaxIterations fires when the runtime reaches its max_iterations limit.
	EventOnMaxIterations EventType = "on_max_iterations"
	// EventOnAgentSwitch fires whenever the runtime moves the active
	// agent to a new one — either delegating a task (transfer_task),
	// handing off the conversation (handoff), or returning to the
	// caller after a transferred task completes. Observational; useful
	// for audit, transcript, and metrics pipelines that track which
	// agent ran which tools without subscribing to the runtime event
	// channel.
	EventOnAgentSwitch EventType = "on_agent_switch"
	// EventOnSessionResume fires when the user explicitly approves the
	// runtime to continue past its configured max_iterations limit.
	// Observational; useful for alerting on extended-runtime sessions
	// or for pipelines that bill / quota-track per resume.
	EventOnSessionResume EventType = "on_session_resume"
	// EventOnToolApprovalDecision fires after the runtime's tool
	// approval chain (yolo / permissions / readonly / ask) has resolved
	// a verdict for a tool call, before the call is executed (for
	// allow) or its error response is recorded (for deny / canceled).
	// Observational; gives audit pipelines a single, structured "who
	// approved what" record without re-implementing the chain.
	EventOnToolApprovalDecision EventType = "on_tool_approval_decision"
	// EventBeforeCompaction fires immediately before a session compaction
	// runs. The hook can:
	//   - veto the compaction by returning Decision == "block" (the runtime
	//     skips compaction entirely);
	//   - replace the LLM-generated summary by returning a non-empty
	//     [HookSpecificOutput.Summary] (the runtime applies that summary
	//     verbatim and skips the model call).
	// The Input carries [Input.InputTokens], [Input.OutputTokens],
	// [Input.ContextLimit] and [Input.CompactionReason] ("threshold",
	// "overflow", or "manual") so handlers can decide based on real
	// session pressure.
	//
	// [Input.ContextLimit] may be 0 when the model definition is
	// unavailable (e.g. an unknown model ID); hooks should treat 0 as
	// "unknown" rather than as a real limit.
	//
	// Hook authors should be cautious about denying when
	// CompactionReason == "overflow": the runtime is recovering from a
	// context-overflow error. A denial here means the next LLM call will
	// hit the same overflow; the runtime allows at most one
	// retry-with-compaction (see maxOverflowCompactions in loop.go), so a
	// second denial fails the turn and surfaces the overflow as an Error
	// event.
	EventBeforeCompaction EventType = "before_compaction"
	// EventAfterCompaction fires after a session compaction completes
	// successfully (a summary was applied to the session). The Input
	// carries the produced [Input.Summary] together with the
	// *pre-compaction* [Input.InputTokens] / [Input.OutputTokens] (what
	// was summarized) so observability handlers can naturally express
	// "compacted from X to Y". The post-compaction counts are reflected
	// in the next runtime token-usage event. AfterCompaction is purely
	// observational; output is ignored.
	EventAfterCompaction EventType = "after_compaction"
	// EventToolResponseTransform fires between a tool's exec and the
	// runtime's emission/record of the response. Hooks may rewrite the
	// tool's textual output by setting
	// [HookSpecificOutput.UpdatedToolResponse]; the runtime applies the
	// rewrite before the response is emitted, recorded, fed to the
	// post_tool_use hook, or sent to the LLM on the next turn. This is
	// the symmetric counterpart of pre_tool_use's UpdatedInput, applied
	// to tool RESULTS instead of tool ARGUMENTS — useful for output
	// scrubbing (the redact_secrets builtin uses it for that), or for
	// truncating excessively long results before they hit the next
	// model call.
	//
	// Tool-scoped: matchers select which tools the hook runs against,
	// like pre_tool_use / post_tool_use.
	EventToolResponseTransform EventType = "tool_response_transform"
)

// Input is the JSON-serializable payload passed to hooks via stdin.
type Input struct {
	SessionID     string    `json:"session_id"`
	Cwd           string    `json:"cwd"`
	HookEventName EventType `json:"hook_event_name"`

	// AgentName identifies the agent dispatching the event. Useful for
	// builtin hooks that need to look up per-agent state via a runtime
	// closure (e.g. response cache).
	AgentName string `json:"agent_name,omitempty"`

	// ModelID identifies the model the runtime is about to call (for
	// before_llm_call) or just called (for after_llm_call), in the
	// canonical "<provider>/<model>" form expected by
	// [modelsdev.Store.GetModel]. Populated from the loop's resolved
	// model so it reflects per-tool model overrides and alloy-mode
	// random selection — do NOT call Agent.Model() from a hook to
	// recompute it, since alloy mode would re-randomize and a per-tool
	// override would be invisible. Empty for events that aren't
	// model-call-scoped.
	ModelID string `json:"model_id,omitempty"`

	// Iteration is the 1-based run-loop iteration counter for the
	// model call this dispatch is gating. Populated for
	// [EventBeforeLLMCall] (1 for the first call of the RunStream, 2
	// for the second, ...); zero for events not tied to a loop
	// iteration. The max_iterations builtin compares it to a configured
	// budget without per-session state.
	Iteration int `json:"iteration,omitempty"`

	// LastUserMessage is the text content of the latest user message in
	// the session at dispatch time. Populated for events that respond to
	// a user turn (stop, after_llm_call). Empty for events that aren't
	// turn-scoped (session_start, session_end, notification, ...).
	LastUserMessage string `json:"last_user_message,omitempty"`

	// Tool-related fields (PreToolUse, PostToolUse, PermissionRequest,
	// ToolResponseTransform).
	ToolName  string         `json:"tool_name,omitempty"`
	ToolUseID string         `json:"tool_use_id,omitempty"`
	ToolInput map[string]any `json:"tool_input,omitempty"`

	// PostToolUse / ToolResponseTransform: the tool's textual output.
	// On post_tool_use it carries the (already-rewritten) response a
	// tool_response_transform hook produced — hooks scrubbing secrets
	// upstream are visible to downstream observers without a second
	// pass.
	ToolResponse any  `json:"tool_response,omitempty"`
	ToolError    bool `json:"tool_error,omitempty"`

	// Messages is the conversation snapshot the runtime is about to send
	// to the model. Populated only for [EventBeforeLLMCall] dispatches
	// where rewriting is meaningful; nil for every other event so the
	// JSON wire payload doesn't carry the whole transcript on every
	// observational hook. Hooks that want to rewrite the messages return
	// the result in [HookSpecificOutput.UpdatedMessages]; the runtime
	// applies the rewrite before the actual provider call.
	Messages []chat.Message `json:"messages,omitempty"`

	// SessionStart specific: "startup", "resume", "clear", "compact".
	// PreCompact specific: "manual", "auto", "overflow", "tool_overflow".
	Source string `json:"source,omitempty"`
	// SessionEnd specific: "clear", "logout", "prompt_input_exit", "other".
	// TurnEnd specific: "normal", "continue", "steered", "error",
	// "canceled", "hook_blocked", "loop_detected".
	Reason string `json:"reason,omitempty"`
	// Stop / AfterLLMCall / SubagentStop: the model's final response content.
	StopResponse string `json:"stop_response,omitempty"`
	// UserPromptSubmit specific: the text the user just submitted.
	Prompt string `json:"prompt,omitempty"`
	// SubagentStop populates [Input.AgentName] (above) with the name of
	// the sub-agent that just finished.
	// SubagentStop specific: ID of the parent session that spawned the sub-agent.
	ParentSessionID string `json:"parent_session_id,omitempty"`
	// Notification specific.
	NotificationLevel   string `json:"notification_level,omitempty"`
	NotificationMessage string `json:"notification_message,omitempty"`

	// OnAgentSwitch specific: the agent the runtime is moving away
	// from (FromAgent) and the one it's switching to (ToAgent), plus
	// the cause of the transition ("transfer_task", "handoff",
	// "transfer_task_return"). Empty FromAgent is valid for the
	// initial switch into the team's default agent.
	FromAgent       string `json:"from_agent,omitempty"`
	ToAgent         string `json:"to_agent,omitempty"`
	AgentSwitchKind string `json:"agent_switch_kind,omitempty"`

	// FromAgentModels is the snapshot of the previous agent's
	// configured model endpoints, captured at on_agent_switch dispatch
	// time. Populated only on [EventOnAgentSwitch]; nil for every other
	// event. Hooks that act on the previous agent's models (e.g. the
	// stock `unload` builtin asking a local inference engine to release
	// GPU memory) read this slice instead of poking at the runtime,
	// keeping the hook payload self-contained.
	FromAgentModels []ModelEndpoint `json:"from_agent_models,omitempty"`

	// OnSessionResume specific: the iteration cap that was reached
	// (PreviousMaxIterations) and the new cap after the user approved
	// continuation (NewMaxIterations). Carrying both lets audit
	// pipelines compute how much extra runtime was granted without
	// reconstructing it from the iteration counter.
	PreviousMaxIterations int `json:"previous_max_iterations,omitempty"`
	NewMaxIterations      int `json:"new_max_iterations,omitempty"`

	// OnToolApprovalDecision specific: the verdict resolved by the
	// approval chain ("allow", "deny", "canceled") and a stable
	// classifier for what produced it ("yolo",
	// "session_permissions_allow", "session_permissions_deny",
	// "team_permissions_allow", "team_permissions_deny",
	// "readonly_hint", "user_approved", "user_approved_session",
	// "user_approved_tool", "user_rejected", "context_canceled").
	ApprovalDecision string `json:"approval_decision,omitempty"`
	ApprovalSource   string `json:"approval_source,omitempty"`

	// Compaction fields (BeforeCompaction, AfterCompaction).
	InputTokens  int64 `json:"input_tokens,omitempty"`
	OutputTokens int64 `json:"output_tokens,omitempty"`
	// ContextLimit is the model's context-window size in tokens. It is
	// 0 when the model definition is unavailable (e.g. an unknown
	// model ID); hooks should treat 0 as "unknown" rather than as a
	// real limit.
	ContextLimit int64 `json:"context_limit,omitempty"`
	// CompactionReason is one of "threshold", "overflow", "manual".
	CompactionReason string `json:"compaction_reason,omitempty"`
	// Summary is the produced compaction summary text. It is populated
	// only on AfterCompaction (BeforeCompaction fires before any
	// summary exists); on AfterCompaction it carries the actual text
	// applied to the session so observability handlers can audit /
	// archive what was summarized.
	Summary string `json:"summary,omitempty"`
}

// ModelEndpoint identifies one of an agent's configured models plus
// the HTTP endpoint that hosts it, when one is known. It is the wire
// format used by [Input.FromAgentModels] so hooks can reach a
// model-serving endpoint without depending on runtime-only types.
type ModelEndpoint struct {
	// Provider is the provider type ("openai", "anthropic", "dmr", ...).
	Provider string `json:"provider,omitempty"`
	// Model is the resolved model identifier.
	Model string `json:"model,omitempty"`
	// BaseURL is the resolved HTTP base URL of the provider, when known
	// (set by providers that talk to a configurable HTTP endpoint, e.g.
	// Docker Model Runner). Empty for cloud providers that don't expose
	// a stable per-instance base URL on the runtime side.
	BaseURL string `json:"base_url,omitempty"`
	// UnloadAPI is the per-model unload path or absolute URL copied
	// verbatim from the model's `unload_api` provider option. Empty
	// when the user hasn't configured an override; the unload builtin
	// falls back to a provider-specific default in that case.
	UnloadAPI string `json:"unload_api,omitempty"`
}

// ToJSON serializes the input.
func (i *Input) ToJSON() ([]byte, error) { return json.Marshal(i) }

// ErrorPolicy controls what happens when a non-fail-closed hook fails.
type ErrorPolicy string

const (
	ErrorPolicyWarn   ErrorPolicy = "warn"
	ErrorPolicyIgnore ErrorPolicy = "ignore"
	ErrorPolicyBlock  ErrorPolicy = "block"
)

type Decision string

const (
	DecisionAllow Decision = "allow"
	DecisionDeny  Decision = "deny"
	DecisionAsk   Decision = "ask"
)

// NewAdditionalContextOutput is a small helper for in-process [BuiltinFunc]
// implementations that just want to contribute additional context for a
// given event. Returning the result of this helper is equivalent to
// returning a fully-populated [Output] with [HookSpecificOutput] set.
func NewAdditionalContextOutput(event EventType, content string) *Output {
	if content == "" {
		return nil
	}
	return &Output{
		HookSpecificOutput: &HookSpecificOutput{
			HookEventName:     event,
			AdditionalContext: content,
		},
	}
}

// Output is the JSON-decoded output of a hook.
type Output struct {
	// Continue indicates whether to continue execution (default: true).
	Continue *bool `json:"continue,omitempty"`
	// StopReason is shown when continue=false.
	StopReason string `json:"stop_reason,omitempty"`
	// SuppressOutput hides stdout from transcript.
	SuppressOutput bool `json:"suppress_output,omitempty"`
	// SystemMessage is a warning to show the user.
	SystemMessage string `json:"system_message,omitempty"`
	// Decision is for blocking operations ("block", ...).
	// In-process builtin hooks should use [DecisionBlockValue].
	Decision string `json:"decision,omitempty"`
	// Reason explains the decision.
	Reason string `json:"reason,omitempty"`
	// HookSpecificOutput contains event-specific fields.
	HookSpecificOutput *HookSpecificOutput `json:"hook_specific_output,omitempty"`
}

// ShouldContinue reports whether execution should continue.
func (o *Output) ShouldContinue() bool { return o.Continue == nil || *o.Continue }

// DecisionBlockValue is the canonical value of [Output.Decision] used
// by hooks to signal a deny/terminate verdict on the current event.
const DecisionBlockValue = "block"

// IsBlocked reports whether the decision is "block".
func (o *Output) IsBlocked() bool { return o.Decision == DecisionBlockValue }

// HookSpecificOutput holds event-specific output fields.
type HookSpecificOutput struct {
	HookEventName EventType `json:"hook_event_name,omitempty"`

	// PreToolUse fields.
	PermissionDecision       Decision       `json:"permission_decision,omitempty"`
	PermissionDecisionReason string         `json:"permission_decision_reason,omitempty"`
	UpdatedInput             map[string]any `json:"updated_input,omitempty"`

	// PostToolUse / SessionStart / TurnStart / Stop fields.
	AdditionalContext string `json:"additional_context,omitempty"`

	// BeforeCompaction: when non-empty, the runtime applies this string as
	// the compaction summary verbatim and skips the LLM-based
	// summarization. Ignored on every other event.
	Summary string `json:"summary,omitempty"`

	// UpdatedMessages, when non-empty on a [EventBeforeLLMCall]
	// response, replaces the chat history the runtime is about to send
	// to the model. Use it for in-process content rewrites that have to
	// happen on every model call — e.g. the redact_secrets builtin
	// scrubbing outbound chat content. Hooks for other events should
	// leave it nil; aggregate() only honours it for before_llm_call.
	//
	// First non-empty wins when multiple before_llm_call hooks return
	// rewrites concurrently — see aggregate(). Compose multiple
	// rewriters into a single hook if you need them to chain.
	UpdatedMessages []chat.Message `json:"updated_messages,omitempty"`

	// UpdatedToolResponse, when non-nil on a
	// [EventToolResponseTransform] response, replaces the tool's
	// textual output before the runtime emits it, records it in the
	// session, hands it to post_tool_use, or sends it to the next LLM
	// call. Pointer-typed so an explicit empty string ("clear the
	// output") is distinguishable from "don't touch it" (nil).
	//
	// First non-nil wins when multiple tool_response_transform hooks
	// return rewrites concurrently — see aggregate(). Compose multiple
	// rewriters into a single hook if you need them to chain.
	UpdatedToolResponse *string `json:"updated_tool_response,omitempty"`
}

// Result is the aggregated outcome of dispatching one event.
type Result struct {
	// Allowed indicates if the operation should proceed.
	Allowed bool
	// PermissionAllowed is set when a [EventPermissionRequest] hook
	// returned permission_decision="allow". The runtime treats this as
	// an explicit auto-approve and skips the interactive confirmation.
	PermissionAllowed bool
	// Message is feedback to include in the response.
	Message string
	// ModifiedInput contains modifications to tool input (PreToolUse).
	ModifiedInput map[string]any
	// AdditionalContext is context added by the hooks.
	AdditionalContext string
	// SystemMessage is a warning to show the user.
	SystemMessage string
	// ExitCode is the worst exit code seen (0 = success, 2 = blocking error, -1 = exec failure).
	ExitCode int
	// Stderr captures stderr from a failing hook.
	Stderr string
	// Summary is set by EventBeforeCompaction hooks to override the
	// LLM-generated compaction summary. When multiple hooks return a
	// non-empty summary, the first one wins.
	Summary string

	// UpdatedMessages is the rewritten chat history produced by an
	// [EventBeforeLLMCall] hook. The runtime sends this slice to the
	// model in place of the original messages. nil means "no rewrite";
	// the runtime falls back to the original messages.
	UpdatedMessages []chat.Message

	// UpdatedToolResponse is the rewritten tool output produced by an
	// [EventToolResponseTransform] hook. The runtime swaps it into the
	// tool's response before emission, recording, post_tool_use, and
	// the next LLM call. Pointer-typed so callers can distinguish
	// "explicitly cleared" (empty string) from "no rewrite" (nil).
	UpdatedToolResponse *string

	// Decision is the most-restrictive PreToolUse verdict reported by
	// any matching hook in the chain ("" when no hook produced one).
	// Most-restrictive ordering: Deny > Ask > Allow > "".
	//
	// The runtime's tool-approval flow consults this BEFORE asking the
	// user, so an LLM-judge hook that returns Allow can auto-approve a
	// call that would otherwise prompt, and Ask can force a prompt for
	// a call that would otherwise auto-run.
	//
	// Always empty for non-PreToolUse events.
	Decision Decision
	// DecisionReason is the human-readable rationale paired with
	// Decision (the reason from the most-restrictive hook). Empty when
	// Decision is empty.
	DecisionReason string
}
