package anthropic

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/tools"
)

// interleavedThinkingEvents is a tool_use turn exercising every shape the
// flattened chat.Message cannot express: two signed thinking blocks (one
// with the signature split across deltas), a redacted block, an omitted
// block (signature but no text), and text/tool_use interleaving.
func interleavedThinkingEvents(withMessageStart bool) []ssestream.Event {
	var events []ssestream.Event
	if withMessageStart {
		events = append(events, sseEvent("message_start", map[string]any{
			"type":    "message_start",
			"message": map[string]any{"id": "msg_raw", "model": "claude-test", "role": "assistant", "type": "message", "content": []any{}},
		}))
	}
	start := func(index int, block map[string]any) ssestream.Event {
		return sseEvent("content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": block})
	}
	delta := func(index int, d map[string]any) ssestream.Event {
		return sseEvent("content_block_delta", map[string]any{"type": "content_block_delta", "index": index, "delta": d})
	}
	stop := func(index int) ssestream.Event {
		return sseEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
	}
	events = append(events,
		start(0, map[string]any{"type": "thinking", "thinking": "", "signature": ""}),
		delta(0, map[string]any{"type": "thinking_delta", "thinking": "first <plan>"}),
		delta(0, map[string]any{"type": "thinking_delta", "thinking": " & more"}),
		delta(0, map[string]any{"type": "signature_delta", "signature": "sig1-part1"}),
		delta(0, map[string]any{"type": "signature_delta", "signature": "-part2"}),
		stop(0),
		start(1, map[string]any{"type": "redacted_thinking", "data": "REDACTED_BLOB"}),
		stop(1),
		start(2, map[string]any{"type": "text", "text": ""}),
		delta(2, map[string]any{"type": "text_delta", "text": "Let me look."}),
		stop(2),
		start(3, map[string]any{"type": "thinking", "thinking": "", "signature": ""}),
		delta(3, map[string]any{"type": "signature_delta", "signature": "sig-omitted"}),
		stop(3),
		start(4, map[string]any{"type": "tool_use", "id": "toolu_A", "name": "read_file", "input": map[string]any{}}),
		delta(4, map[string]any{"type": "input_json_delta", "partial_json": `{"path":`}),
		delta(4, map[string]any{"type": "input_json_delta", "partial_json": `"a.go"}`}),
		stop(4),
		start(5, map[string]any{"type": "text", "text": ""}),
		delta(5, map[string]any{"type": "text_delta", "text": "and"}),
		stop(5),
		start(6, map[string]any{"type": "tool_use", "id": "toolu_B", "name": "list_dir", "input": map[string]any{}}),
		delta(6, map[string]any{"type": "input_json_delta", "partial_json": `{"dir":"."}`}),
		stop(6),
		sseEvent("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}, "usage": map[string]any{"output_tokens": 42}}),
		sseEvent("message_stop", map[string]any{"type": "message_stop"}),
	)
	return events
}

const interleavedThinkingWire = `[
	{"type":"thinking","thinking":"first <plan> & more","signature":"sig1-part1-part2"},
	{"type":"redacted_thinking","data":"REDACTED_BLOB"},
	{"type":"text","text":"Let me look."},
	{"type":"thinking","thinking":"","signature":"sig-omitted"},
	{"type":"tool_use","id":"toolu_A","name":"read_file","input":{"path":"a.go"}},
	{"type":"text","text":"and"},
	{"type":"tool_use","id":"toolu_B","name":"list_dir","input":{"dir":"."}}
]`

// drainStream aggregates adapter chunks the way pkg/runtime/streaming.go
// does, returning the assistant message the runtime would persist.
func drainStream(t *testing.T, stream chat.MessageStream) chat.Message {
	t.Helper()
	msg := chat.Message{Role: chat.MessageRoleAssistant}
	var content, reasoning strings.Builder
	index := map[string]int{}
	var state *chat.ProviderState
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		if len(resp.Choices) == 0 {
			continue
		}
		d := resp.Choices[0].Delta
		content.WriteString(d.Content)
		reasoning.WriteString(d.ReasoningContent)
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
	msg.Content = content.String()
	msg.ReasoningContent = reasoning.String()
	msg.AttachProviderState(state)
	return msg
}

func drainStandard(t *testing.T, events []ssestream.Event) chat.Message {
	t.Helper()
	stream := ssestream.NewStream[anthropic.MessageStreamEventUnion](&fakeDecoder{events: events}, nil)
	return drainStream(t, (&Client{}).newStreamAdapter(stream, true))
}

func drainBeta(t *testing.T, events []ssestream.Event) chat.Message {
	t.Helper()
	stream := ssestream.NewStream[anthropic.BetaRawMessageStreamEventUnion](&fakeDecoder{events: events}, nil)
	return drainStream(t, (&Client{}).newBetaStreamAdapter(stream, true))
}

func TestStreamAdapter_RecordsRawContentOnMessageStop(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		drain func(*testing.T, []ssestream.Event) chat.Message
	}{
		{"standard", drainStandard},
		{"beta", drainBeta},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			msg := tc.drain(t, interleavedThinkingEvents(true))

			// The flattened view is lossy: one merged reasoning string, one signature.
			assert.Equal(t, "Let me look.and", msg.Content)
			assert.Equal(t, "first <plan> & more", msg.ReasoningContent)
			assert.Equal(t, "sig-omitted", msg.ThinkingSignature)
			require.Len(t, msg.ToolCalls, 2)

			require.NotNil(t, msg.ProviderState)
			assert.Equal(t, providerStateName, msg.ProviderState.Provider)
			assert.Equal(t, "msg_raw", msg.ProviderState.MessageID)
			assert.NotEmpty(t, msg.ProviderState.ContentHash)
			assert.JSONEq(t, interleavedThinkingWire, string(msg.ProviderState.Content))
		})
	}
}

func TestStreamAdapter_RawContentWithoutMessageStart(t *testing.T) {
	t.Parallel()

	msg := drainStandard(t, interleavedThinkingEvents(false))
	require.NotNil(t, msg.ProviderState)
	assert.Empty(t, msg.ProviderState.MessageID)
	assert.JSONEq(t, interleavedThinkingWire, string(msg.ProviderState.Content))

	msg = drainBeta(t, interleavedThinkingEvents(false))
	require.NotNil(t, msg.ProviderState)
	assert.JSONEq(t, interleavedThinkingWire, string(msg.ProviderState.Content))
}

func TestStreamAdapter_NoRawContentForEmptyResponse(t *testing.T) {
	t.Parallel()

	events := []ssestream.Event{
		sseEvent("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_e", "role": "assistant", "type": "message", "content": []any{}}}),
		sseEvent("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}, "usage": map[string]any{"output_tokens": 1}}),
		sseEvent("message_stop", map[string]any{"type": "message_stop"}),
	}
	msg := drainStandard(t, events)
	assert.Nil(t, msg.ProviderState)
}

func TestStreamAdapter_AccumulateFailureKeepsStreaming(t *testing.T) {
	t.Parallel()

	// Block index 1 with nothing at index 0 makes the SDK accumulator fail;
	// the flattened stream must be unaffected and no raw state recorded.
	events := []ssestream.Event{
		sseEvent("content_block_start", map[string]any{"type": "content_block_start", "index": 1, "content_block": map[string]any{"type": "text", "text": ""}}),
		sseEvent("content_block_delta", map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "text_delta", "text": "hi"}}),
		sseEvent("message_stop", map[string]any{"type": "message_stop"}),
	}
	msg := drainStandard(t, events)
	assert.Equal(t, "hi", msg.Content)
	assert.Nil(t, msg.ProviderState)
}

func TestReplayContent_ExactRoundTrip(t *testing.T) {
	t.Parallel()

	msg := drainStandard(t, interleavedThinkingEvents(true))

	blocks, ok, err := replayContent(&msg, false)
	require.NoError(t, err)
	require.True(t, ok)
	got, err := json.Marshal(blocks)
	require.NoError(t, err)
	assert.JSONEq(t, interleavedThinkingWire, string(got))

	betaBlocks, ok, err := betaReplayContent(&msg, false)
	require.NoError(t, err)
	require.True(t, ok)
	got, err = json.Marshal(betaBlocks)
	require.NoError(t, err)
	assert.JSONEq(t, interleavedThinkingWire, string(got))
}

func TestReplayContent_SurvivesJSONRoundTrip(t *testing.T) {
	t.Parallel()

	msg := drainStandard(t, interleavedThinkingEvents(true))
	data, err := json.Marshal(msg)
	require.NoError(t, err)
	var restored chat.Message
	require.NoError(t, json.Unmarshal(data, &restored))

	blocks, ok, err := replayContent(&restored, false)
	require.NoError(t, err)
	require.True(t, ok)
	got, err := json.Marshal(blocks)
	require.NoError(t, err)
	assert.JSONEq(t, interleavedThinkingWire, string(got))
}

func TestReplayContent_FallsBackWhenNotReplayable(t *testing.T) {
	t.Parallel()

	cases := map[string]func(*chat.Message){
		"no state":               func(m *chat.Message) { m.ProviderState = nil },
		"other provider":         func(m *chat.Message) { m.ProviderState.Provider = "openai" },
		"unsealed state":         func(m *chat.Message) { m.ProviderState.ContentHash = "" },
		"user edited text":       func(m *chat.Message) { m.Content += " (edited)" },
		"hook redacted thinking": func(m *chat.Message) { m.ReasoningContent = "[REDACTED]" },
		"hook rewrote arguments": func(m *chat.Message) { m.ToolCalls[0].Function.Arguments = `{"path":"/etc/passwd"}` },
		"sanitized tool name": func(m *chat.Message) {
			m.ToolCalls[0].Function.Name = "read_file_safe"
			m.AttachProviderState(m.ProviderState)
		},
		"refusal dropped calls": func(m *chat.Message) {
			m.ToolCalls = nil
			m.AttachProviderState(m.ProviderState)
		},
		"xml fallback added calls": func(m *chat.Message) {
			m.ToolCalls = append(m.ToolCalls, tools.ToolCall{ID: "xml_1", Function: tools.FunctionCall{Name: "extra"}})
			m.AttachProviderState(m.ProviderState)
		},
		"calls reordered": func(m *chat.Message) {
			m.ToolCalls[0], m.ToolCalls[1] = m.ToolCalls[1], m.ToolCalls[0]
			m.AttachProviderState(m.ProviderState)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			msg := drainStandard(t, interleavedThinkingEvents(true))
			mutate(&msg)

			blocks, ok, err := replayContent(&msg, false)
			require.NoError(t, err)
			assert.False(t, ok)
			assert.Nil(t, blocks)

			betaBlocks, ok, err := betaReplayContent(&msg, false)
			require.NoError(t, err)
			assert.False(t, ok)
			assert.Nil(t, betaBlocks)
		})
	}
}

// The runtime seals the hash after it has already reshaped the message
// (media-file markers filtered out of the text, tool names sanitized), so
// the hash alone cannot tell the raw blocks drifted from what is persisted.
func TestReplayContent_FallsBackWhenRawDriftedBeforeSealing(t *testing.T) {
	t.Parallel()

	cases := map[string]func(*chat.Message){
		"media marker stripped from text": func(m *chat.Message) { m.Content = "Let me look." },
		"reasoning rewritten":             func(m *chat.Message) { m.ReasoningContent = "[REDACTED]" },
		"arguments rewritten":             func(m *chat.Message) { m.ToolCalls[0].Function.Arguments = `{"path":"/etc/passwd"}` },
		"tool name sanitized":             func(m *chat.Message) { m.ToolCalls[0].Function.Name = "read_file_safe" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			msg := drainStandard(t, interleavedThinkingEvents(true))
			mutate(&msg)
			msg.AttachProviderState(msg.ProviderState)
			require.NotNil(t, msg.ReplayableProviderState(providerStateName), "the hash matches the mutated message")

			_, ok, err := replayContent(&msg, false)
			require.NoError(t, err)
			assert.False(t, ok)
			_, ok, err = betaReplayContent(&msg, false)
			require.NoError(t, err)
			assert.False(t, ok)
		})
	}
}

func TestReplayContent_ArgumentsCompareAsJSON(t *testing.T) {
	t.Parallel()

	// Whitespace and key order are not edits.
	msg := drainStandard(t, interleavedThinkingEvents(true))
	msg.ToolCalls[0].Function.Arguments = `{ "path" : "a.go" }`
	msg.AttachProviderState(msg.ProviderState)
	_, ok, err := replayContent(&msg, false)
	require.NoError(t, err)
	assert.True(t, ok)

	// A cut-off tool call leaves unparsable arguments and an empty raw input;
	// both convert to an empty object.
	msg = chat.Message{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{{ID: "toolu_C", Function: tools.FunctionCall{Name: "read_file", Arguments: `{"path":`}}}}
	msg.AttachProviderState(&chat.ProviderState{Provider: providerStateName, Content: json.RawMessage(`[{"type":"tool_use","id":"toolu_C","name":"read_file","input":{}}]`)})
	blocks, ok, err := replayContent(&msg, false)
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, blocks, 1)
	assert.Equal(t, "toolu_C", blocks[0].OfToolUse.ID)
}

func TestReplayContent_RejectsMalformedState(t *testing.T) {
	t.Parallel()

	sealed := func(raw string) chat.Message {
		msg := chat.Message{Role: chat.MessageRoleAssistant, Content: "x"}
		msg.AttachProviderState(&chat.ProviderState{Provider: providerStateName, Content: json.RawMessage(raw)})
		return msg
	}
	for name, raw := range map[string]string{
		"invalid json":       `[{"type":"text",`,
		"not an array":       `{"type":"text","text":"x"}`,
		"unknown block type": `[{"type":"text","text":"x"},{"type":"hologram","data":"?"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			msg := sealed(raw)
			_, ok, err := replayContent(&msg, false)
			require.Error(t, err)
			assert.False(t, ok)
			_, ok, err = betaReplayContent(&msg, false)
			require.Error(t, err)
			assert.False(t, ok)
		})
	}
}

func TestReplayContent_SkipsBlocksTheAPIRejects(t *testing.T) {
	t.Parallel()

	msg := chat.Message{Role: chat.MessageRoleAssistant, Content: "done", ReasoningContent: "cut off"}
	msg.AttachProviderState(&chat.ProviderState{Provider: providerStateName, Content: json.RawMessage(`[
		{"type":"thinking","thinking":"cut off","signature":""},
		{"type":"text","text":""},
		{"type":"text","text":"done"}
	]`)})

	blocks, ok, err := replayContent(&msg, false)
	require.NoError(t, err)
	require.True(t, ok)
	got, err := json.Marshal(blocks)
	require.NoError(t, err)
	assert.JSONEq(t, `[{"type":"text","text":"done"}]`, string(got))

	// Nothing left to send is "not replayable", not an error.
	msg.ProviderState = nil
	msg.AttachProviderState(&chat.ProviderState{Provider: providerStateName, Content: json.RawMessage(`[{"type":"text","text":""}]`)})
	blocks, ok, err = replayContent(&msg, false)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Nil(t, blocks)
}

func TestReplayContent_OmittedThinkingStaysThinking(t *testing.T) {
	t.Parallel()

	// thinking_display: omitted returns signed thinking blocks with no text.
	// They must go back as thinking blocks, not redacted_thinking.
	msg := chat.Message{Role: chat.MessageRoleAssistant, Content: "ok", ThinkingSignature: "sig-omitted"}
	msg.AttachProviderState(&chat.ProviderState{Provider: providerStateName, Content: json.RawMessage(`[
		{"type":"thinking","thinking":"","signature":"sig-omitted"},
		{"type":"text","text":"ok"}
	]`)})

	blocks, ok, err := replayContent(&msg, false)
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, blocks, 2)
	require.NotNil(t, blocks[0].OfThinking)
	assert.Equal(t, "sig-omitted", blocks[0].OfThinking.Signature)
	assert.Empty(t, blocks[0].OfThinking.Thinking)
	assert.Nil(t, blocks[0].OfRedactedThinking)
}
