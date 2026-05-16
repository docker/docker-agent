package compaction

import (
	_ "embed"
	"slices"

	"github.com/docker/docker-agent/pkg/chat"
)

var (
	//go:embed prompts/compaction-system.txt
	SystemPrompt string

	//go:embed prompts/compaction-user.txt
	UserPrompt string
)

// contextThreshold is the fraction of the context window at which compaction
// is triggered. When the estimated token usage exceeds this fraction of the
// context limit, compaction is recommended.
const contextThreshold = 0.9

// ShouldCompact reports whether a session's context usage has crossed the
// compaction threshold. It returns true when the total token count
// (input + output + addedTokens) exceeds [contextThreshold] (90%) of
// contextLimit.
func ShouldCompact(inputTokens, outputTokens, addedTokens, contextLimit int64) bool {
	if contextLimit <= 0 {
		return false
	}
	return (inputTokens + outputTokens + addedTokens) > int64(float64(contextLimit)*contextThreshold)
}

// EstimateMessageTokens returns a rough token-count estimate for a single
// chat message based on its text length. This is intentionally conservative
// (overestimates) so that proactive compaction fires before we hit the limit.
//
// The estimate accounts for message content, multi-content text parts,
// reasoning content, tool call arguments, and a small per-message overhead
// for role/metadata tokens.
func EstimateMessageTokens(msg *chat.Message) int64 {
	// charsPerToken: average characters per token. 4 is a widely-used
	// heuristic for English; slightly overestimates for code/JSON (~3.5).
	const charsPerToken = 4

	// perMessageOverhead: role, ToolCallID, delimiters, etc.
	const perMessageOverhead = 5

	var chars int
	chars += len(msg.Content)
	for _, part := range msg.MultiContent {
		chars += len(part.Text)
	}
	chars += len(msg.ReasoningContent)
	for _, tc := range msg.ToolCalls {
		chars += len(tc.Function.Arguments)
		chars += len(tc.Function.Name)
	}

	if chars == 0 {
		return perMessageOverhead
	}
	return int64(chars/charsPerToken) + perMessageOverhead
}

// SplitIndexForKeep walks messages from the end and returns the earliest
// index whose suffix fits in maxTokens, snapping to user/assistant
// boundaries. All messages from the returned index onward are intended
// to be preserved verbatim across a compaction; messages before it are
// the candidates to summarize. Returns len(messages) when everything
// fits in the keep budget — i.e. compact everything.
//
// The boundary snap matters for providers (notably Anthropic) that
// reject conversations starting on a tool-result message: by stopping
// the kept window on a user/assistant turn, we guarantee the kept
// suffix begins on a clean conversational turn.
func SplitIndexForKeep(messages []chat.Message, maxTokens int64) int {
	if len(messages) == 0 {
		return 0
	}

	var tokens int64
	lastValidBoundary := len(messages)
	for i := range slices.Backward(messages) {
		tokens += EstimateMessageTokens(&messages[i])
		if tokens > maxTokens {
			return lastValidBoundary
		}
		role := messages[i].Role
		if role == chat.MessageRoleUser || role == chat.MessageRoleAssistant {
			lastValidBoundary = i
		}
	}
	return len(messages)
}

// FirstIndexInBudget returns the smallest index N such that
// messages[N:] fits within contextLimit, snapping to a user/assistant
// turn boundary. Used to truncate the conversation handed to the
// summarization model so the request itself doesn't blow the context
// window.
//
// When the entire slice fits within contextLimit, the function returns
// the index of the earliest user/assistant message in the suffix —
// older tool-only messages (which can't legally start a conversation)
// are dropped. In the unusual case of a tool-only conversation with
// no user/asst turns, it returns len(messages); callers should treat
// that as "nothing to send" and skip the truncation.
func FirstIndexInBudget(messages []chat.Message, contextLimit int64) int {
	var tokens int64
	lastValidMessageSeen := len(messages)
	for i := range slices.Backward(messages) {
		tokens += EstimateMessageTokens(&messages[i])
		if tokens > contextLimit {
			return lastValidMessageSeen
		}
		role := messages[i].Role
		if role == chat.MessageRoleUser || role == chat.MessageRoleAssistant {
			lastValidMessageSeen = i
		}
	}
	return lastValidMessageSeen
}
