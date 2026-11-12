package bedrock

import (
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/tools"
)

// accumulate builds the assistant message from streamed deltas with the rules
// of pkg/runtime/streaming.go: content and reasoning are concatenated, the
// signature is last-wins, tool calls merge by ID with concatenated arguments,
// and the provider state comes from the terminal chunk.
func accumulate(t *testing.T, deltas []chat.MessageDelta) chat.Message {
	t.Helper()
	msg := chat.Message{Role: chat.MessageRoleAssistant}
	var state *chat.ProviderState
	index := map[string]int{}
	for _, d := range deltas {
		msg.Content += d.Content
		msg.ReasoningContent += d.ReasoningContent
		if d.ThinkingSignature != "" {
			msg.ThinkingSignature = d.ThinkingSignature
		}
		if d.ProviderState != nil {
			state = d.ProviderState
		}
		for _, tc := range d.ToolCalls {
			i, ok := index[tc.ID]
			if !ok {
				i = len(msg.ToolCalls)
				index[tc.ID] = i
				msg.ToolCalls = append(msg.ToolCalls, tools.ToolCall{ID: tc.ID, Type: tc.Type})
			}
			if tc.Function.Name != "" {
				msg.ToolCalls[i].Function.Name = tc.Function.Name
			}
			msg.ToolCalls[i].Function.Arguments += tc.Function.Arguments
		}
	}
	msg.AttachProviderState(state)
	return msg
}

func TestNativeReasoning_RoundTrip(t *testing.T) {
	t.Parallel()

	data := []byte{0xff, 0x00, 0x80}
	deltas, err := drain(t, scriptedAdapter(nil,
		reasoningText(0, "plan "), reasoningText(0, "it"), reasoningSignature(0, "s1"), blockStop(0),
		toolStart(1, "t1", "read"), toolInputDelta(1, `{"path":`), toolInputDelta(1, `"a"}`), blockStop(1),
		reasoningRedacted(2, data), blockStop(2),
		reasoningSignature(3, "s2"), blockStop(3),
		textDelta(4, "done"), blockStop(4),
		messageStop(types.StopReasonToolUse), metadata(),
	))
	require.NoError(t, err)

	produced := accumulate(t, deltas)
	require.NotNil(t, produced.ReplayableProviderState(providerName), "the producing turn must be replayable")

	raw, err := json.Marshal(produced)
	require.NoError(t, err)
	var reloaded chat.Message
	require.NoError(t, json.Unmarshal(raw, &reloaded))

	blocks := convertAssistantContent(&reloaded)
	require.Len(t, blocks, 5)

	first, ok := reasoningOf(t, blocks[0]).(*types.ReasoningContentBlockMemberReasoningText)
	require.True(t, ok)
	assert.Equal(t, "plan it", *first.Value.Text)
	assert.Equal(t, "s1", *first.Value.Signature)

	tool, ok := blocks[1].(*types.ContentBlockMemberToolUse)
	require.True(t, ok)
	assert.Equal(t, "t1", *tool.Value.ToolUseId)

	redacted, ok := reasoningOf(t, blocks[2]).(*types.ReasoningContentBlockMemberRedactedContent)
	require.True(t, ok)
	assert.Equal(t, data, redacted.Value)

	signedEmpty, ok := reasoningOf(t, blocks[3]).(*types.ReasoningContentBlockMemberReasoningText)
	require.True(t, ok)
	assert.Empty(t, *signedEmpty.Value.Text)
	assert.Equal(t, "s2", *signedEmpty.Value.Signature)

	text, ok := blocks[4].(*types.ContentBlockMemberText)
	require.True(t, ok)
	assert.Equal(t, "done", text.Value)
}
