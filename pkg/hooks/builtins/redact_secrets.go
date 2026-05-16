package builtins

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	"github.com/docker/portcullis"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/hooks"
)

// RedactSecrets is the registered name of the builtin that scrubs
// secret material at every leak vector docker-agent has into a third
// party. The same builtin is registered once and dispatches on
// [hooks.Input.HookEventName] so a single name covers all three legs:
//
//   - [hooks.EventPreToolUse]            — scrub tool ARGUMENTS before
//     the call leaves the runtime (returns UpdatedInput).
//   - [hooks.EventBeforeLLMCall]         — scrub outgoing CHAT CONTENT
//     before each model call (returns UpdatedMessages).
//   - [hooks.EventToolResponseTransform] — scrub tool OUTPUT before it
//     reaches event consumers, the persisted session, the
//     post_tool_use hook, or the next LLM call (returns
//     UpdatedToolResponse).
//
// The agent-level redact_secrets flag is shorthand for wiring the
// builtin into all three events at once via [ApplyAgentDefaults]; the
// same hook entries can be authored directly in YAML to opt in to
// individual legs (or all of them) without touching agent flags.
//
// Configured under any other event the builtin returns nil and logs a
// warning: keeping it lenient avoids surprising failures from typos
// while still nudging users toward the right event.
const RedactSecrets = "redact_secrets"

// redactSecrets is the [hooks.BuiltinFunc] registered under
// [RedactSecrets]. It dispatches on the event so YAML callers can
// reuse the same `command: redact_secrets` entry across all three
// legs of the feature without remembering separate names.
func redactSecrets(_ context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
	if in == nil {
		return nil, nil
	}
	switch in.HookEventName {
	case hooks.EventPreToolUse:
		return redactToolArgs(in), nil
	case hooks.EventBeforeLLMCall:
		return redactOutgoingMessages(in), nil
	case hooks.EventToolResponseTransform:
		return redactToolOutput(in), nil
	default:
		// Lenient on misconfiguration: log once per dispatch so the
		// user sees the typo, but don't fail the run loop. The
		// alternative (returning an error) would short-circuit the
		// entire event for fail-closed-event types like pre_tool_use,
		// which is wildly disproportionate to "registered the builtin
		// under the wrong key".
		slog.Warn("redact_secrets builtin invoked under unsupported event; no-op",
			"event", in.HookEventName)
		return nil, nil
	}
}

// redactToolArgs walks every value in [hooks.Input.ToolInput]
// (recursively into nested maps and slices) and replaces secret spans
// with [portcullis.Marker]. When nothing matched it returns
// a nil [hooks.Output] so unaffected tool calls take the cheap path
// through the executor.
//
// The returned UpdatedInput contains ONLY keys whose value was
// actually rewritten. This matters because pre_tool_use hooks run
// concurrently and aggregate via shallow maps.Copy: emitting unchanged
// keys would clobber another hook's modifications on the same input.
func redactToolArgs(in *hooks.Input) *hooks.Output {
	if len(in.ToolInput) == 0 {
		return nil
	}
	updated := redactToolInput(in.ToolInput)
	if len(updated) == 0 {
		return nil
	}
	return &hooks.Output{
		SystemMessage: fmt.Sprintf("redact_secrets: redacted secret material from arguments of tool %q", in.ToolName),
		HookSpecificOutput: &hooks.HookSpecificOutput{
			HookEventName: hooks.EventPreToolUse,
			UpdatedInput:  updated,
		},
	}
}

// redactOutgoingMessages scrubs every text-bearing field of every
// message in [hooks.Input.Messages] and returns the rewrite via
// [HookSpecificOutput.UpdatedMessages]. Returns nil for an empty input
// or when nothing changed so the runtime takes the no-rewrite fast path.
//
// Always returns the full message slice (not a delta) because the
// runtime swaps in the entire slice; partial returns would silently
// drop the un-rewritten messages.
func redactOutgoingMessages(in *hooks.Input) *hooks.Output {
	if len(in.Messages) == 0 {
		return nil
	}
	out := make([]chat.Message, len(in.Messages))
	changed := false
	for i, m := range in.Messages {
		out[i] = redactMessage(m)
		if !changed && messageChanged(m, out[i]) {
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return &hooks.Output{
		HookSpecificOutput: &hooks.HookSpecificOutput{
			HookEventName:   hooks.EventBeforeLLMCall,
			UpdatedMessages: out,
		},
	}
}

// redactToolOutput scrubs the tool's textual response. The payload is
// carried as `any` in the wire protocol because some MCP tools may
// produce structured shapes; the dispatcher's record path only feeds
// strings to the LLM today, so anything else is out of scope.
//
// Returning a pointer-typed [HookSpecificOutput.UpdatedToolResponse]
// preserves the empty-string-vs-no-rewrite distinction (see the field
// docs); a redaction that wipes the entire output to "" is still
// honoured by the runtime.
func redactToolOutput(in *hooks.Input) *hooks.Output {
	if in.ToolResponse == nil {
		return nil
	}
	original, ok := in.ToolResponse.(string)
	if !ok {
		return nil
	}
	scrubbed := portcullis.Redact(original)
	if scrubbed == original {
		return nil
	}
	return &hooks.Output{
		HookSpecificOutput: &hooks.HookSpecificOutput{
			HookEventName:       hooks.EventToolResponseTransform,
			UpdatedToolResponse: &scrubbed,
		},
	}
}

// redactToolInput returns a map containing only the top-level keys of
// m whose redacted value differs from the original. Returns nil when
// nothing changed so the caller can short-circuit cheaply.
func redactToolInput(m map[string]any) map[string]any {
	var changed map[string]any
	for k, v := range m {
		nv, c := redactAny(v)
		if !c {
			continue
		}
		if changed == nil {
			changed = make(map[string]any, 1)
		}
		changed[k] = nv
	}
	return changed
}

// redactAny recursively scrubs secrets out of v, returning the new
// value and a "did anything change" flag. Strings go through
// [portcullis.Redact]; map[string]any / []any are walked to catch
// secrets nested inside JSON-style payloads. Every other Go type
// passes through unchanged because the scanner only operates on text.
func redactAny(v any) (any, bool) {
	switch val := v.(type) {
	case string:
		redacted := portcullis.Redact(val)
		return redacted, redacted != val
	case map[string]any:
		out := make(map[string]any, len(val))
		changed := false
		for k, item := range val {
			nv, c := redactAny(item)
			out[k] = nv
			changed = changed || c
		}
		return out, changed
	case []any:
		out := make([]any, len(val))
		changed := false
		for i, item := range val {
			nv, c := redactAny(item)
			out[i] = nv
			changed = changed || c
		}
		return out, changed
	default:
		return v, false
	}
}

// redactMessage scrubs every text-bearing field of m that round-trips
// to a model provider:
//
//   - [chat.Message.Content]
//   - [chat.Message.ReasoningContent] (sent back to Anthropic, Bedrock,
//     DeepSeek as a thinking block, so a previous turn's reasoning
//     trace must not leak a secret it mentioned)
//   - text parts of [chat.Message.MultiContent]
//   - the legacy singular [chat.Message.FunctionCall].Arguments
//     (still sent by the OpenAI provider when set)
//   - the JSON-encoded arguments of every entry in
//     [chat.Message.ToolCalls]
//
// Other fields (image URLs, file references, ThinkingSignature,
// ThoughtSignature) are not scanned: they're either opaque provider
// tokens or non-text payloads outside the portcullis ruleset's reach.
//
// MultiContent and ToolCalls slices are cloned (and FunctionCall
// pointers are deep-copied) before being mutated so the caller's
// message history is left untouched. We don't bother with
// copy-on-write: portcullis.Redact is essentially free on inputs
// that don't match a rule (it returns the same string), so the only
// cost on a clean conversation is the per-message slice/struct clone
// — negligible compared to the LLM call this rewrite is gating.
func redactMessage(m chat.Message) chat.Message {
	m.Content = portcullis.Redact(m.Content)
	m.ReasoningContent = portcullis.Redact(m.ReasoningContent)

	if len(m.MultiContent) > 0 {
		m.MultiContent = slices.Clone(m.MultiContent)
		for i := range m.MultiContent {
			if m.MultiContent[i].Type == chat.MessagePartTypeText {
				m.MultiContent[i].Text = portcullis.Redact(m.MultiContent[i].Text)
			}
		}
	}

	if m.FunctionCall != nil {
		fc := *m.FunctionCall
		fc.Arguments = portcullis.Redact(fc.Arguments)
		m.FunctionCall = &fc
	}

	if len(m.ToolCalls) > 0 {
		m.ToolCalls = slices.Clone(m.ToolCalls)
		for i := range m.ToolCalls {
			m.ToolCalls[i].Function.Arguments = portcullis.Redact(m.ToolCalls[i].Function.Arguments)
		}
	}

	return m
}

// messageChanged reports whether redactMessage rewrote any text-bearing
// field of orig into rewritten. We compare only the fields redactMessage
// touches; everything else is a structural copy and equal by construction.
//
// The comparison is field-wise (not via reflect.DeepEqual) so the hot
// path stays branchy-cheap on a clean conversation: every comparison
// short-circuits on the first hit.
func messageChanged(orig, rewritten chat.Message) bool {
	if orig.Content != rewritten.Content {
		return true
	}
	if orig.ReasoningContent != rewritten.ReasoningContent {
		return true
	}
	if len(orig.MultiContent) != len(rewritten.MultiContent) {
		return true
	}
	for i := range orig.MultiContent {
		if orig.MultiContent[i].Type == chat.MessagePartTypeText &&
			orig.MultiContent[i].Text != rewritten.MultiContent[i].Text {
			return true
		}
	}
	if (orig.FunctionCall == nil) != (rewritten.FunctionCall == nil) {
		return true
	}
	if orig.FunctionCall != nil && orig.FunctionCall.Arguments != rewritten.FunctionCall.Arguments {
		return true
	}
	if len(orig.ToolCalls) != len(rewritten.ToolCalls) {
		return true
	}
	for i := range orig.ToolCalls {
		if orig.ToolCalls[i].Function.Arguments != rewritten.ToolCalls[i].Function.Arguments {
			return true
		}
	}
	return false
}
