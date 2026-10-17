package oaistream

/*
This file holds the policy for replaying stored assistant reasoning to an
OpenAI-compatible endpoint, and the message rewrite that policy requires.
*/

import (
	"log/slog"
	"slices"
	"sync"

	"github.com/docker/docker-agent/pkg/chat"
)

// ProviderOptReplayReasoning is the provider_opts key that overrides, per
// model, whether stored assistant reasoning is replayed as a reasoning_content
// field on Chat Completions requests. A boolean; when unset, replay defaults
// on. It does not apply to the Responses API, where
// provider_opts.preserve_reasoning governs the replay of encrypted reasoning
// items instead.
const ProviderOptReplayReasoning = "replay_reasoning"

// ReplayReasoning reports whether assistant reasoning stored in the session may
// be replayed to a model's endpoint. Replay defaults on. An explicit
// provider_opts.replay_reasoning overrides the default. A
// value that is present but not a boolean turns replay off instead of falling
// back to the default: the option is how an operator keeps reasoning away from
// an endpoint, so a typo must not leave replay enabled.
//
// Replaying [chat.Message.ReasoningContent] lets a reasoning model see its own
// previous reasoning after a tool call (issue #4363). Because reasoning_content
// is a non-standard extension, the explicit false value is the compatibility
// escape hatch for endpoints that reject or must not receive it.
func ReplayReasoning(providerOpts map[string]any) bool {
	value, present := providerOpts[ProviderOptReplayReasoning]
	if !present {
		return true
	}
	if enabled, ok := value.(bool); ok {
		return enabled
	}
	warnInvalidReplayReasoning.Do(func() {
		slog.Warn("Rejecting provider_opts.replay_reasoning: value is not a boolean; assistant reasoning will not be replayed", "value", value)
	})
	return false
}

// warnInvalidReplayReasoning keeps the diagnostic above to once per process:
// the option is read on every request.
var warnInvalidReplayReasoning sync.Once

// WithoutReplayedReasoning returns messages with assistant reasoning cleared,
// for a model that opted out with [ProviderOptReplayReasoning]. Only the
// reasoning field is
// affected: content, tool calls, and media are left alone, so an assistant turn
// that had both reasoning and content still converts to a valid message.
//
// The input is never mutated -- the slice is copied only when something has to
// change -- because the messages belong to the session, not to the provider.
func WithoutReplayedReasoning(messages []chat.Message) []chat.Message {
	out := messages
	copied := false
	for i := range messages {
		if messages[i].Role != chat.MessageRoleAssistant || messages[i].ReasoningContent == "" {
			continue
		}
		if !copied {
			out = slices.Clone(messages)
			copied = true
		}
		out[i].ReasoningContent = ""
	}
	return out
}
