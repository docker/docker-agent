package bedrock

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/tools"
)

// sealedMessage builds the message the runtime accumulates from these blocks
// (concatenated text and reasoning, last signature) and seals the raw state.
func sealedMessage(t *testing.T, blocks []nativeBlock) chat.Message {
	t.Helper()
	msg := chat.Message{Role: chat.MessageRoleAssistant}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			msg.Content += b.Text
		case "reasoning":
			msg.ReasoningContent += b.Text
			msg.ThinkingSignature = b.Signature
		case "tool_use":
			msg.ToolCalls = append(msg.ToolCalls, tools.ToolCall{ID: b.ID, Type: "function", Function: tools.FunctionCall{Name: b.Name, Arguments: string(b.Input)}})
		}
	}
	content, err := json.Marshal(blocks)
	require.NoError(t, err)
	msg.AttachProviderState(&chat.ProviderState{Provider: providerName, Content: content})
	return msg
}

var interleaved = []nativeBlock{
	{Type: "reasoning", Text: "a", Signature: "s1"},
	{Type: "text", Text: "hi"},
	{Type: "tool_use", ID: "t1", Name: "f", Input: json.RawMessage(`{"x":1}`)},
	{Type: "reasoning", Signature: "s2"},
}

func reasoningOf(t *testing.T, b types.ContentBlock) types.ReasoningContentBlock {
	t.Helper()
	r, ok := b.(*types.ContentBlockMemberReasoningContent)
	require.True(t, ok, "expected a reasoning block, got %T", b)
	return r.Value
}

func TestConvertAssistantContent_ReplaysNativeBlocks(t *testing.T) {
	t.Parallel()

	msg := sealedMessage(t, interleaved)
	blocks := convertAssistantContent(&msg)

	require.Len(t, blocks, 4)
	first, ok := reasoningOf(t, blocks[0]).(*types.ReasoningContentBlockMemberReasoningText)
	require.True(t, ok)
	assert.Equal(t, "a", *first.Value.Text)
	assert.Equal(t, "s1", *first.Value.Signature)

	text, ok := blocks[1].(*types.ContentBlockMemberText)
	require.True(t, ok)
	assert.Equal(t, "hi", text.Value)

	tool, ok := blocks[2].(*types.ContentBlockMemberToolUse)
	require.True(t, ok)
	assert.Equal(t, "t1", *tool.Value.ToolUseId)
	assert.Equal(t, "f", *tool.Value.Name)

	last, ok := reasoningOf(t, blocks[3]).(*types.ReasoningContentBlockMemberReasoningText)
	require.True(t, ok)
	assert.Empty(t, *last.Value.Text)
	assert.Equal(t, "s2", *last.Value.Signature)
}

func TestConvertAssistantContent_RedactedBytesSurvive(t *testing.T) {
	t.Parallel()

	data := []byte{0xff, 0x00, 0x80}
	msg := sealedMessage(t, []nativeBlock{{Type: "redacted_reasoning", Data: data}, {Type: "text", Text: "ok"}})

	raw, err := json.Marshal(msg)
	require.NoError(t, err)
	var reloaded chat.Message
	require.NoError(t, json.Unmarshal(raw, &reloaded))

	blocks := convertAssistantContent(&reloaded)
	require.Len(t, blocks, 2)
	redacted, ok := reasoningOf(t, blocks[0]).(*types.ReasoningContentBlockMemberRedactedContent)
	require.True(t, ok)
	assert.Equal(t, data, redacted.Value)
}

func TestConvertAssistantContent_RecoversWhenTextEdited(t *testing.T) {
	t.Parallel()

	msg := sealedMessage(t, interleaved)
	msg.Content = "edited"

	// The reasoning is unchanged, so the recorded blocks are replayed in their
	// original order with the edited text substituted.
	blocks := convertAssistantContent(&msg)
	require.Len(t, blocks, 4)
	_, ok := reasoningOf(t, blocks[0]).(*types.ReasoningContentBlockMemberReasoningText)
	require.True(t, ok)
	text, ok := blocks[1].(*types.ContentBlockMemberText)
	require.True(t, ok)
	assert.Equal(t, "edited", text.Value)
	_, ok = blocks[2].(*types.ContentBlockMemberToolUse)
	assert.True(t, ok)
	_, ok = reasoningOf(t, blocks[3]).(*types.ReasoningContentBlockMemberReasoningText)
	assert.True(t, ok)
}

// Unsigned reasoning (thinking cut off by max_tokens, or another vendor's
// reasoning on Bedrock) cannot be verified, so it is never replayed. It still
// counts toward the message match, as on the Anthropic replay path.
func TestConvertAssistantContent_SkipsUnsignedReasoning(t *testing.T) {
	t.Parallel()

	msg := sealedMessage(t, []nativeBlock{
		{Type: "reasoning", Text: "a", Signature: "s1"},
		{Type: "reasoning", Text: "cut"},
		{Type: "text", Text: "hi"},
	})
	blocks := convertAssistantContent(&msg)

	require.Len(t, blocks, 2)
	signed, ok := reasoningOf(t, blocks[0]).(*types.ReasoningContentBlockMemberReasoningText)
	require.True(t, ok)
	assert.Equal(t, "s1", *signed.Value.Signature)
	_, ok = blocks[1].(*types.ContentBlockMemberText)
	assert.True(t, ok)
}

// Not parallel: AllocsPerRun counts process-wide allocations.
func TestBlockRecorder_AccumulatesDeltasWithoutPerDeltaCopies(t *testing.T) {
	const n = 1000
	textDelta := &types.ContentBlockDeltaMemberText{Value: "x"}
	reasoningDelta := &types.ContentBlockDeltaMemberReasoningContent{Value: &types.ReasoningContentBlockDeltaMemberText{Value: "y"}}

	var r blockRecorder
	allocs := testing.AllocsPerRun(1, func() {
		r = blockRecorder{}
		for range n {
			r.delta(0, reasoningDelta)
			r.delta(1, textDelta)
		}
	})
	assert.Less(t, allocs, float64(n/10), "deltas must not copy the accumulated block")

	r.delta(0, &types.ContentBlockDeltaMemberReasoningContent{Value: &types.ReasoningContentBlockDeltaMemberSignature{Value: "sig"}})
	r.stop(0)
	r.stop(1)
	state := r.state()
	require.NotNil(t, state)
	var blocks []nativeBlock
	require.NoError(t, json.Unmarshal(state.Content, &blocks))
	require.Len(t, blocks, 2)
	assert.Equal(t, nativeBlock{Type: "reasoning", Text: strings.Repeat("y", n), Signature: "sig"}, blocks[0])
	assert.Equal(t, nativeBlock{Type: "text", Text: strings.Repeat("x", n)}, blocks[1])
}

// editedNativeTurn is a Bedrock tool-use turn whose recorded blocks no longer
// match it (e.g. a hook redacted its reasoning).
func editedNativeTurn(t *testing.T) chat.Message {
	t.Helper()
	msg := sealedMessage(t, []nativeBlock{
		{Type: "reasoning", Text: "t", Signature: "s"},
		{Type: "tool_use", ID: "t1", Name: "read", Input: json.RawMessage(`{}`)},
	})
	msg.ReasoningContent = "[REDACTED]"
	return msg
}

func TestUnreplayableNativeBlocks(t *testing.T) {
	t.Parallel()

	user := chat.Message{Role: chat.MessageRoleUser, Content: "q"}
	result := chat.Message{Role: chat.MessageRoleTool, ToolCallID: "t1", Content: "ok"}

	t.Run("edited turn omits thinking", func(t *testing.T) {
		t.Parallel()
		msg := editedNativeTurn(t)
		for _, b := range convertAssistantContent(&msg) {
			_, isReasoning := b.(*types.ContentBlockMemberReasoningContent)
			assert.False(t, isReasoning)
		}
	})

	t.Run("edited turn in the active loop is refused when thinking", func(t *testing.T) {
		t.Parallel()
		client := bedrockThinkingClient("anthropic.claude-sonnet-5-5", &latest.ThinkingBudget{Effort: "high"})
		_, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{user, editedNativeTurn(t), result}, nil)
		require.ErrorContains(t, err, "changed after it was recorded and cannot be replayed")
	})

	t.Run("earlier edited turn is not refused", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, checkReplayableThinking("m", true, []chat.Message{user, editedNativeTurn(t), result, user}))
	})

	t.Run("thinking off is not refused", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, checkReplayableThinking("m", false, []chat.Message{user, editedNativeTurn(t), result}))
	})
}

// Runtime transforms change a turn's tool calls without touching its thinking:
// the recorded reasoning is still replayed, and nothing is refused.
func TestIntactReasoningSurvivesRuntimeTransforms(t *testing.T) {
	t.Parallel()

	turn := func(t *testing.T, args string) chat.Message {
		t.Helper()
		msg := sealedMessage(t, []nativeBlock{
			{Type: "reasoning", Text: "t", Signature: "s"},
			{Type: "tool_use", ID: "t1", Name: `read" path="x`, Input: json.RawMessage(`{"token":"abc"}`)},
		})
		msg.ToolCalls[0].Function.Name = "read"
		msg.AttachProviderState(msg.ProviderState)
		msg.ToolCalls[0].Function.Arguments = args
		return msg
	}
	user := chat.Message{Role: chat.MessageRoleUser, Content: "q"}
	result := chat.Message{Role: chat.MessageRoleTool, ToolCallID: "t1", Content: "ok"}

	for name, args := range map[string]string{"sanitized tool name": `{"token":"abc"}`, "redacted arguments": `{"token":"[REDACTED]"}`} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			msg := turn(t, args)
			require.NoError(t, checkReplayableThinking("m", true, []chat.Message{user, msg, result}))

			blocks := convertAssistantContent(&msg)
			require.Len(t, blocks, 2)
			reasoning, ok := reasoningOf(t, blocks[0]).(*types.ReasoningContentBlockMemberReasoningText)
			require.True(t, ok)
			assert.Equal(t, "s", *reasoning.Value.Signature)
			tool, ok := blocks[1].(*types.ContentBlockMemberToolUse)
			require.True(t, ok)
			assert.Equal(t, "read", *tool.Value.Name, "the sanitized tool call is sent")
		})
	}
}

func TestRecordedReasoningRecovery(t *testing.T) {
	t.Parallel()

	user := chat.Message{Role: chat.MessageRoleUser, Content: "q"}
	result := func(id string) chat.Message {
		return chat.Message{Role: chat.MessageRoleTool, ToolCallID: id, Content: "ok"}
	}
	call := func(id, name string) tools.ToolCall {
		return tools.ToolCall{ID: id, Type: "function", Function: tools.FunctionCall{Name: name, Arguments: `{}`}}
	}
	// recorded seals msg with blocks at version, then applies edit.
	recorded := func(t *testing.T, blocks []nativeBlock, version int, msg chat.Message, edit func(*chat.Message)) chat.Message {
		t.Helper()
		content, err := json.Marshal(blocks)
		require.NoError(t, err)
		msg.Role = chat.MessageRoleAssistant
		msg.AttachProviderState(&chat.ProviderState{Provider: providerName, Version: version, Content: content})
		if edit != nil {
			edit(&msg)
		}
		return msg
	}
	sanitize := func(m *chat.Message) {
		for i := range m.ToolCalls {
			m.ToolCalls[i].Function.Name = "read"
		}
	}
	blockTypes := func(blocks []types.ContentBlock) []string {
		var out []string
		for _, b := range blocks {
			out = append(out, fmt.Sprintf("%T", b))
		}
		return out
	}

	t.Run("split signature is recovered", func(t *testing.T) {
		t.Parallel()
		msg := recorded(t, []nativeBlock{{Type: "reasoning", Text: "t", Signature: "s1s2"}, {Type: "tool_use", ID: "t1", Name: `read" x`, Input: json.RawMessage(`{}`)}}, 1,
			chat.Message{ReasoningContent: "t", ThinkingSignature: "s2", ToolCalls: []tools.ToolCall{call("t1", `read" x`)}}, sanitize)
		require.NoError(t, checkReplayableThinking("m", true, []chat.Message{user, msg, result("t1")}))
		blocks := convertAssistantContent(&msg)
		reasoning, ok := reasoningOf(t, blocks[0]).(*types.ReasoningContentBlockMemberReasoningText)
		require.True(t, ok)
		assert.Equal(t, "s1s2", *reasoning.Value.Signature)
	})

	t.Run("interleaved reasoning keeps its order", func(t *testing.T) {
		t.Parallel()
		msg := recorded(t, []nativeBlock{
			{Type: "reasoning", Text: "a", Signature: "s1"},
			{Type: "tool_use", ID: "t1", Name: `read" x`, Input: json.RawMessage(`{}`)},
			{Type: "reasoning", Text: "b", Signature: "s2"},
			{Type: "tool_use", ID: "t2", Name: `read" y`, Input: json.RawMessage(`{}`)},
		}, 1, chat.Message{ReasoningContent: "ab", ThinkingSignature: "s2", ToolCalls: []tools.ToolCall{call("t1", `read" x`), call("t2", `read" y`)}}, sanitize)
		require.NoError(t, checkReplayableThinking("m", true, []chat.Message{user, msg, result("t1"), result("t2")}))
		blocks := convertAssistantContent(&msg)
		assert.Equal(t, []string{
			"*types.ContentBlockMemberReasoningContent", "*types.ContentBlockMemberToolUse",
			"*types.ContentBlockMemberReasoningContent", "*types.ContentBlockMemberToolUse",
		}, blockTypes(blocks))
		tool, ok := blocks[1].(*types.ContentBlockMemberToolUse)
		require.True(t, ok)
		assert.Equal(t, "read", *tool.Value.Name)
	})

	t.Run("redacted-only reasoning is refused when it cannot be recovered", func(t *testing.T) {
		t.Parallel()
		msg := recorded(t, []nativeBlock{{Type: "redacted_reasoning", Data: []byte{1}}, {Type: "tool_use", ID: "t1", Name: "read", Input: json.RawMessage(`{}`)}}, 1,
			chat.Message{ToolCalls: []tools.ToolCall{call("t1", "read")}}, func(m *chat.Message) { m.ToolCalls = append(m.ToolCalls, call("t2", "read")) })
		require.ErrorContains(t, checkReplayableThinking("m", true, []chat.Message{user, msg, result("t1"), result("t2")}), "cannot be replayed")
	})

	t.Run("cleared signature with intact reasoning is recovered", func(t *testing.T) {
		t.Parallel()
		msg := recorded(t, []nativeBlock{{Type: "reasoning", Text: "t", Signature: "s"}, {Type: "tool_use", ID: "t1", Name: "read", Input: json.RawMessage(`{}`)}}, 1,
			chat.Message{ReasoningContent: "t", ThinkingSignature: "s", ToolCalls: []tools.ToolCall{call("t1", "read")}}, func(m *chat.Message) { m.ThinkingSignature = "" })
		require.NoError(t, checkReplayableThinking("m", true, []chat.Message{user, msg, result("t1")}))
		reasoning, ok := reasoningOf(t, convertAssistantContent(&msg)[0]).(*types.ReasoningContentBlockMemberReasoningText)
		require.True(t, ok)
		assert.Equal(t, "s", *reasoning.Value.Signature)
	})

	t.Run("newer state version is refused in the loop", func(t *testing.T) {
		t.Parallel()
		msg := recorded(t, []nativeBlock{{Type: "reasoning", Text: "t", Signature: "s"}, {Type: "tool_use", ID: "t1", Name: "read", Input: json.RawMessage(`{}`)}}, chat.ProviderStateVersion+1,
			chat.Message{ReasoningContent: "t", ThinkingSignature: "s", ToolCalls: []tools.ToolCall{call("t1", "read")}}, nil)
		require.ErrorContains(t, checkReplayableThinking("m", true, []chat.Message{user, msg, result("t1")}), "cannot be replayed")
	})
}
