package oaistream

import (
"testing"

"github.com/stretchr/testify/assert"
"github.com/stretchr/testify/require"

"github.com/docker/docker-agent/pkg/chat"
"github.com/docker/docker-agent/pkg/modelsdev"
"github.com/docker/docker-agent/pkg/tools"
)

// TestReproIssue4363_ReasoningContentDroppedOnReplay reproduces
// https://github.com/docker/docker-agent/issues/4363.
//
// For OpenAI-compatible custom providers (e.g. Qwen served via llama.cpp,
// vLLM, or any other endpoint that speaks the DeepSeek-style
// "reasoning_content" convention -- see the doc comment on
// chat.Message.ReasoningContent), Docker Agent correctly captures streamed
// reasoning, accumulates it, and stores it on the assistant chat.Message.
//
// However, convertMessagesWithCaps built the outgoing
// ChatCompletionAssistantMessageParam from Content, FunctionCall, and
// ToolCalls only -- msg.ReasoningContent was never read, so the reasoning
// was silently dropped when the conversation was replayed on the next
// request. A reasoning model then has no way to see its own prior
// reasoning after a tool call, and must reconstruct it from scratch --
// inflating reasoning tokens and latency on every subsequent turn.
//
// The fix attaches the stored reasoning to the outgoing assistant message
// via SetExtraFields, since openai-go's typed
// ChatCompletionAssistantMessageParam has no native field for it (this is
// a non-standard, provider-specific extension; see openai/openai-go#558).
func TestReproIssue4363_ReasoningContentDroppedOnReplay(t *testing.T) {
t.Parallel()

store := modelsdev.NewDatabaseStore(&modelsdev.Database{})

t.Run("bug: reasoning content is carried onto the replayed assistant message", func(t *testing.T) {
t.Parallel()
messages := []chat.Message{
{
Role:             chat.MessageRoleAssistant,
Content:          "The capital of France is Paris.",
ReasoningContent: "The user asked about France's capital; I recall it is Paris.",
},
}

result := ConvertMessages(t.Context(), messages, modelsdev.ID{}, store, nil)
require.Len(t, result, 1)
require.NotNil(t, result[0].OfAssistant, "expected an assistant message param")

extra := result[0].OfAssistant.ExtraFields()
require.NotNil(t, extra, "expected extra fields to carry reasoning_content")
assert.Equal(t, "The user asked about France's capital; I recall it is Paris.", extra["reasoning_content"],
"issue #4363: stored reasoning must be replayed to OpenAI-compatible providers, not silently dropped")
})

t.Run("no reasoning content: no extra field is added", func(t *testing.T) {
t.Parallel()
messages := []chat.Message{
{Role: chat.MessageRoleAssistant, Content: "Hi there!"},
}

result := ConvertMessages(t.Context(), messages, modelsdev.ID{}, store, nil)
require.Len(t, result, 1)
require.NotNil(t, result[0].OfAssistant)

extra := result[0].OfAssistant.ExtraFields()
assert.NotContains(t, extra, "reasoning_content",
"models with no stored reasoning should not gain a reasoning_content field")
})

t.Run("reasoning content survives alongside tool calls", func(t *testing.T) {
t.Parallel()
messages := []chat.Message{
{
Role:             chat.MessageRoleAssistant,
Content:          "",
ReasoningContent: "I should call the weather tool for this city.",
ToolCalls: []tools.ToolCall{
{ID: "call_1"},
},
},
}

result := ConvertMessages(t.Context(), messages, modelsdev.ID{}, store, nil)
require.Len(t, result, 1)
require.NotNil(t, result[0].OfAssistant)

extra := result[0].OfAssistant.ExtraFields()
assert.Equal(t, "I should call the weather tool for this city.", extra["reasoning_content"])
assert.Len(t, result[0].OfAssistant.ToolCalls, 1, "tool calls must still be converted alongside reasoning")
})
}
