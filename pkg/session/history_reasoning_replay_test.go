package session

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider/oaistream"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/tools"
)

// assistantFromWire marshals a converted request and returns its first
// assistant message as a raw map: reasoning_content is a non-standard
// extension that only exists on the wire.
func assistantFromWire(t *testing.T, msgs any) map[string]any {
	t.Helper()

	raw, err := json.Marshal(msgs)
	require.NoError(t, err)

	var out []map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	for _, m := range out {
		if m["role"] == "assistant" {
			return m
		}
	}
	t.Fatalf("no assistant message in request: %s", raw)
	return nil
}

// TestGetMessages_ReasoningReplayedToRequest is the end-to-end regression for
// issue #4363: reasoning captured on an assistant turn that also carries a tool
// call has to survive session history and then reach the next Chat Completions
// request, so the model does not re-derive reasoning it already produced.
//
// Both halves matter. The history filters (isUsageOnlyMessage,
// normalizeMessageContent) key on content and tool calls, so this turn is
// retained; the converter is what used to drop the reasoning on the floor.
func TestGetMessages_ReasoningReplayedToRequest(t *testing.T) {
	t.Parallel()

	s := New()
	s.AddMessage(UserMessage("what is the weather?"))
	s.AddMessage(NewAgentMessage("test-agent", &chat.Message{
		Role:             chat.MessageRoleAssistant,
		ReasoningContent: "I should call the weather tool",
		ToolCalls:        []tools.ToolCall{{ID: "call_1", Function: tools.FunctionCall{Name: "weather", Arguments: `{"city":"Paris"}`}}},
		Usage:            &chat.Usage{InputTokens: 10, OutputTokens: 5},
	}))
	s.AddMessage(&Message{Message: chat.Message{Role: chat.MessageRoleTool, ToolCallID: "call_1", Content: "sunny, 24C"}})

	history := s.GetMessages(agent.New("test-agent", "test instruction"))

	var stored *chat.Message
	for i := range history {
		if history[i].Role == chat.MessageRoleAssistant {
			stored = &history[i]
		}
	}
	require.NotNil(t, stored, "a reasoning turn that carries a tool call must stay in history")
	assert.Equal(t, "I should call the weather tool", stored.ReasoningContent)

	converted := oaistream.ConvertMessages(t.Context(), history, modelsdev.NewID("custom", "qwen3"), nil, nil)
	assistant := assistantFromWire(t, converted)

	assert.Equal(t, "I should call the weather tool", assistant["reasoning_content"])
	require.Len(t, assistant["tool_calls"], 1)
	calls, ok := assistant["tool_calls"].([]any)
	require.True(t, ok)
	assert.Equal(t, "call_1", calls[0].(map[string]any)["id"])
}

// TestGetMessages_ReasoningOnlyTurnNotReplayed pins the boundary of what issue
// #4363 asks for: a turn whose only payload is reasoning is dropped by the
// history filters before any converter runs, so replaying it is out of scope
// here. Retaining such turns is a separate change with context-size
// consequences.
func TestGetMessages_ReasoningOnlyTurnNotReplayed(t *testing.T) {
	t.Parallel()

	s := New()
	s.AddMessage(UserMessage("think about it"))
	s.AddMessage(NewAgentMessage("test-agent", &chat.Message{
		Role:             chat.MessageRoleAssistant,
		ReasoningContent: "pondering",
		Usage:            &chat.Usage{InputTokens: 10, OutputTokens: 5},
	}))

	history := s.GetMessages(agent.New("test-agent", "test instruction"))
	for _, m := range history {
		assert.NotEqual(t, "pondering", m.ReasoningContent,
			"reasoning-only turns are not retained by session history")
	}
}
