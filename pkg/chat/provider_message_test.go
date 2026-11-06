package chat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

func assistantWithState(t *testing.T) Message {
	t.Helper()
	msg := Message{
		Role:             MessageRoleAssistant,
		Content:          "hello",
		ReasoningContent: "let me think",
		ToolCalls: []tools.ToolCall{{
			ID: "toolu_1", Type: "function",
			Function: tools.FunctionCall{Name: "read_file", Arguments: `{"path":"a"}`},
		}},
	}
	msg.AttachProviderState(&ProviderState{
		Provider:       "anthropic",
		MessageID:      "msg_1",
		Content:        json.RawMessage(`[{"type":"text","text":"hello"}]`),
		RequestContext: json.RawMessage(`{"v":1}`),
	})
	require.NotNil(t, msg.ProviderState)
	require.NotEmpty(t, msg.ProviderState.ContentHash)
	return msg
}

func TestAttachProviderState_CopiesAndSeals(t *testing.T) {
	t.Parallel()

	original := &ProviderState{Provider: "anthropic", Content: json.RawMessage(`[]`)}
	var msg Message
	msg.AttachProviderState(original)

	assert.Empty(t, original.ContentHash, "caller's state must not be mutated")
	assert.NotSame(t, original, msg.ProviderState)
	assert.Equal(t, msg.VisibleContentHash(), msg.ProviderState.ContentHash)

	msg.AttachProviderState(nil)
	assert.Nil(t, msg.ProviderState)
}

func TestReplayableProviderState(t *testing.T) {
	t.Parallel()

	t.Run("unchanged message replays", func(t *testing.T) {
		t.Parallel()
		msg := assistantWithState(t)
		assert.NotNil(t, msg.ReplayableProviderState("anthropic"))
	})

	t.Run("other provider does not replay", func(t *testing.T) {
		t.Parallel()
		msg := assistantWithState(t)
		assert.Nil(t, msg.ReplayableProviderState("openai"))
	})

	t.Run("unsealed state does not replay", func(t *testing.T) {
		t.Parallel()
		msg := assistantWithState(t)
		msg.ProviderState.ContentHash = ""
		assert.Nil(t, msg.ReplayableProviderState("anthropic"))
	})

	edits := map[string]func(*Message){
		"content":        func(m *Message) { m.Content = "hello (edited)" },
		"reasoning":      func(m *Message) { m.ReasoningContent = "[REDACTED]" },
		"tool arguments": func(m *Message) { m.ToolCalls[0].Function.Arguments = `{"path":"b"}` },
		"tool name":      func(m *Message) { m.ToolCalls[0].Function.Name = "write_file" },
		"tool dropped":   func(m *Message) { m.ToolCalls = nil },
	}
	for name, edit := range edits {
		t.Run(name+" edit invalidates state", func(t *testing.T) {
			t.Parallel()
			msg := assistantWithState(t)
			edit(&msg)
			assert.Nil(t, msg.ReplayableProviderState("anthropic"))
		})
	}
}

func TestProviderState_JSONRoundTrip(t *testing.T) {
	t.Parallel()

	msg := assistantWithState(t)
	data, err := json.Marshal(msg)
	require.NoError(t, err)

	var decoded Message
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.NotNil(t, decoded.ProviderState)
	assert.Equal(t, msg.ProviderState.Provider, decoded.ProviderState.Provider)
	assert.Equal(t, msg.ProviderState.MessageID, decoded.ProviderState.MessageID)
	assert.JSONEq(t, string(msg.ProviderState.Content), string(decoded.ProviderState.Content))
	assert.JSONEq(t, string(msg.ProviderState.RequestContext), string(decoded.ProviderState.RequestContext))
	assert.NotNil(t, decoded.ReplayableProviderState("anthropic"))

	var plain Message
	require.NoError(t, json.Unmarshal([]byte(`{"role":"assistant","content":"x"}`), &plain))
	assert.Nil(t, plain.ProviderState)
	assert.Nil(t, plain.ReplayableProviderState("anthropic"))
}

func TestMessageIDDoesNotInvalidateProviderState(t *testing.T) {
	t.Parallel()
	msg := assistantWithState(t)
	before := msg.VisibleContentHash()
	msg.MessageID = "conversation-id"
	assert.Equal(t, before, msg.VisibleContentHash())
	wire, err := json.Marshal(msg)
	require.NoError(t, err)
	var restored Message
	require.NoError(t, json.Unmarshal(wire, &restored))
	assert.Equal(t, msg.MessageID, restored.MessageID)
	assert.Equal(t, "msg_1", restored.ProviderState.MessageID)
}

func TestReplayableProviderState_Version(t *testing.T) {
	t.Parallel()

	for version, want := range map[int]bool{0: true, ProviderStateVersion: true, ProviderStateVersion + 1: false} {
		msg := Message{Role: MessageRoleAssistant, Content: "hi"}
		msg.AttachProviderState(&ProviderState{Provider: "p", Version: version, Content: json.RawMessage(`[{}]`)})
		assert.Equal(t, want, msg.ReplayableProviderState("p") != nil, "version %d", version)
	}
}

func TestHasOwnRawContent(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		state *ProviderState
		want  bool
	}{
		"no state":         {nil, false},
		"foreign provider": {&ProviderState{Provider: "other", Content: json.RawMessage(`[{}]`)}, false},
		"empty array":      {&ProviderState{Provider: "p", Content: json.RawMessage(`[]`)}, false},
		"malformed":        {&ProviderState{Provider: "p", Content: json.RawMessage(`{`)}, false},
		"one block":        {&ProviderState{Provider: "p", Content: json.RawMessage(`[{}]`)}, true},
	} {
		msg := Message{Role: MessageRoleAssistant, ProviderState: tc.state}
		assert.Equal(t, tc.want, msg.HasOwnRawContent("p"), name)
	}
}

func TestActiveToolLoopStart(t *testing.T) {
	t.Parallel()

	u := Message{Role: MessageRoleUser}
	a := Message{Role: MessageRoleAssistant}
	tool := Message{Role: MessageRoleTool}
	assert.Equal(t, 1, ActiveToolLoopStart([]Message{u, a, tool, a, tool}))
	assert.Equal(t, 3, ActiveToolLoopStart([]Message{u, a, u, a, tool, tool}))
	assert.Equal(t, 0, ActiveToolLoopStart([]Message{a, tool}))
}
