package runtime

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// TestResponseAPIToolCallHandling verifies that tool calls from the Response API
// are correctly accumulated even when they arrive with the same ID in multiple chunks
func TestResponseAPIToolCallHandling(t *testing.T) {
	// Simulate how the Response API sends tool calls:
	// 1. First event with ID and name
	// 2. Multiple events with the same ID adding arguments incrementally
	stream := newStreamBuilder().
		AddToolCallName("call_abc", "search").
		AddToolCallArguments("call_abc", `{"que`).
		AddToolCallArguments("call_abc", `ry": "te`).
		AddToolCallArguments("call_abc", `st"}`).
		AddStopWithUsage(10, 15).
		Build()

	sess := session.New(session.WithUserMessage("Search for test"))

	events := runSession(t, sess, stream)

	// Verify that we got the expected partial tool call events
	require.True(t, hasEventType(t, events, &PartialToolCallEvent{}), "Expected PartialToolCallEvent")

	// Verify the session has the complete tool call with full arguments
	messages := sess.GetAllMessages()
	var foundToolCall bool
	for _, msg := range messages {
		if msg.Message.Role == "assistant" && len(msg.Message.ToolCalls) > 0 {
			foundToolCall = true
			require.Equal(t, "call_abc", msg.Message.ToolCalls[0].ID)
			require.Equal(t, "search", msg.Message.ToolCalls[0].Function.Name)
			require.JSONEq(t, `{"query": "test"}`, msg.Message.ToolCalls[0].Function.Arguments)
		}
	}
	require.True(t, foundToolCall, "Expected to find complete tool call in session messages")
}

// TestResponseAPIMultipleToolCalls verifies that multiple tool calls
// from the Response API are correctly tracked independently
func TestResponseAPIMultipleToolCalls(t *testing.T) {
	// Simulate multiple tool calls with interleaved arguments
	stream := newStreamBuilder().
		AddToolCallName("call_1", "search").
		AddToolCallName("call_2", "calculate").
		AddToolCallArguments("call_1", `{"query": "test1"}`).
		AddToolCallArguments("call_2", `{"expression": "2+2"}`).
		AddStopWithUsage(20, 30).
		Build()

	sess := session.New(session.WithUserMessage("Search and calculate"))

	events := runSession(t, sess, stream)

	// Verify that we got partial tool call events
	require.True(t, hasEventType(t, events, &PartialToolCallEvent{}), "Expected PartialToolCallEvent")

	// Verify the session has both complete tool calls
	messages := sess.GetAllMessages()
	var toolCalls []string
	for _, msg := range messages {
		if msg.Message.Role == "assistant" && len(msg.Message.ToolCalls) > 0 {
			for _, tc := range msg.Message.ToolCalls {
				toolCalls = append(toolCalls, tc.Function.Name)
			}
		}
	}
	require.ElementsMatch(t, []string{"search", "calculate"}, toolCalls, "Expected both tool calls")
}

func TestPartialToolCallEventsContainOnlyNewArgumentBytes(t *testing.T) {
	stream := newStreamBuilder().
		AddToolCallName("call_abc", "write_file").
		AddToolCallArguments("call_abc", `{"path":"story.md"`).
		AddToolCallArguments("call_abc", `,"content":"Once upon a time"}`).
		AddStopWithUsage(10, 15).
		Build()

	sess := session.New(session.WithUserMessage("Write a story"))
	events := runSession(t, sess, stream)

	var partials []*PartialToolCallEvent
	for _, event := range events {
		if ev, ok := event.(*PartialToolCallEvent); ok {
			partials = append(partials, ev)
		}
	}

	require.Len(t, partials, 3)
	require.Equal(t, "write_file", partials[0].ToolCall.Function.Name)
	require.Empty(t, partials[0].ToolCall.Function.Arguments)
	require.Equal(t, `{"path":"story.md"`, partials[1].ToolCall.Function.Arguments) //nolint:testifylint // testifylint wants us to use require.JSONEq  but the expected value is not valid JSON
	require.Nil(t, partials[1].ToolDefinition)
	require.Equal(t, `,"content":"Once upon a time"}`, partials[2].ToolCall.Function.Arguments)
	require.Nil(t, partials[2].ToolDefinition)

	secondJSON, err := json.Marshal(partials[1])
	require.NoError(t, err)
	require.NotContains(t, string(secondJSON), `"tool_definition"`)
}

func TestPartialToolCallEventJSONIncludesToolDefinitionOnlyWhenPresent(t *testing.T) {
	toolDef := &tools.Tool{Name: "write_file", Description: "Create file"}
	withDef := &PartialToolCallEvent{
		Type:           "partial_tool_call",
		ToolCall:       tools.ToolCall{ID: "call_1", Type: "function", Function: tools.FunctionCall{Name: "write_file"}},
		ToolDefinition: toolDef,
		AgentContext:   newAgentContext("root"),
	}
	withoutDef := &PartialToolCallEvent{
		Type:         "partial_tool_call",
		ToolCall:     tools.ToolCall{ID: "call_1", Type: "function", Function: tools.FunctionCall{Name: "write_file", Arguments: `{"path":"story.md"}`}},
		AgentContext: newAgentContext("root"),
	}

	withDefJSON, err := json.Marshal(withDef)
	require.NoError(t, err)
	require.Contains(t, string(withDefJSON), `"tool_definition"`)

	withoutDefJSON, err := json.Marshal(withoutDef)
	require.NoError(t, err)
	require.NotContains(t, string(withoutDefJSON), `"tool_definition"`)
}
