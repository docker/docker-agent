package openai

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/cagent/pkg/chat"
	"github.com/docker/cagent/pkg/tools"
)

func TestConvertMessagesToResponseInput_AssistantTextWithToolCalls(t *testing.T) {
	// When an assistant message has both text content AND tool calls,
	// the text content must be preserved as a separate assistant message
	// item before the function call items. Dropping it causes the model
	// to lose conversational context and potentially re-start its approach.
	messages := []chat.Message{
		{Role: chat.MessageRoleUser, Content: "Do something"},
		{
			Role:    chat.MessageRoleAssistant,
			Content: "Let me check that for you.",
			ToolCalls: []tools.ToolCall{
				{
					ID:   "call_1",
					Type: "function",
					Function: tools.FunctionCall{
						Name:      "read_file",
						Arguments: `{"path":"foo.go"}`,
					},
				},
			},
		},
		{Role: chat.MessageRoleTool, ToolCallID: "call_1", Content: "file contents here"},
	}

	input := convertMessagesToResponseInput(messages)

	require.Len(t, input, 4, "should have user + assistant text + function_call + function_call_output")

	// Item 0: user message
	assert.NotNil(t, input[0].OfMessage)
	assert.Equal(t, "Do something", input[0].OfMessage.Content.OfString.Value)

	// Item 1: assistant text content (preserved, not dropped)
	require.NotNil(t, input[1].OfMessage, "assistant text should be emitted as a separate message")
	assert.Equal(t, "Let me check that for you.", input[1].OfMessage.Content.OfString.Value)

	// Item 2: function call
	require.NotNil(t, input[2].OfFunctionCall)
	assert.Equal(t, "call_1", input[2].OfFunctionCall.CallID)
	assert.Equal(t, "read_file", input[2].OfFunctionCall.Name)

	// Item 3: function call output
	require.NotNil(t, input[3].OfFunctionCallOutput)
	assert.Equal(t, "call_1", input[3].OfFunctionCallOutput.CallID)
}

func TestConvertMessagesToResponseInput_AssistantToolCallsOnly(t *testing.T) {
	// When assistant has tool calls but no text, no extra message should be emitted.
	messages := []chat.Message{
		{Role: chat.MessageRoleUser, Content: "Do something"},
		{
			Role: chat.MessageRoleAssistant,
			ToolCalls: []tools.ToolCall{
				{
					ID:   "call_1",
					Type: "function",
					Function: tools.FunctionCall{
						Name:      "read_file",
						Arguments: `{"path":"foo.go"}`,
					},
				},
			},
		},
		{Role: chat.MessageRoleTool, ToolCallID: "call_1", Content: "file contents"},
	}

	input := convertMessagesToResponseInput(messages)

	require.Len(t, input, 3, "should have user + function_call + function_call_output (no extra assistant message)")

	assert.NotNil(t, input[0].OfMessage)
	assert.NotNil(t, input[1].OfFunctionCall)
	assert.NotNil(t, input[2].OfFunctionCallOutput)
}

func TestConvertMessagesToResponseInput_MultipleToolCalls(t *testing.T) {
	// Verify that multiple tool calls from a single assistant message
	// all get emitted, and text content is preserved.
	messages := []chat.Message{
		{Role: chat.MessageRoleUser, Content: "Check these files"},
		{
			Role:    chat.MessageRoleAssistant,
			Content: "I'll read both files.",
			ToolCalls: []tools.ToolCall{
				{ID: "call_1", Type: "function", Function: tools.FunctionCall{Name: "read_file", Arguments: `{"path":"a.go"}`}},
				{ID: "call_2", Type: "function", Function: tools.FunctionCall{Name: "read_file", Arguments: `{"path":"b.go"}`}},
			},
		},
		{Role: chat.MessageRoleTool, ToolCallID: "call_1", Content: "contents of a"},
		{Role: chat.MessageRoleTool, ToolCallID: "call_2", Content: "contents of b"},
	}

	input := convertMessagesToResponseInput(messages)

	// user + assistant text + 2 function_calls + 2 function_call_outputs = 6
	require.Len(t, input, 6)

	assert.NotNil(t, input[0].OfMessage)                                                // user
	assert.NotNil(t, input[1].OfMessage)                                                // assistant text
	assert.Equal(t, "I'll read both files.", input[1].OfMessage.Content.OfString.Value) // assistant text preserved
	assert.NotNil(t, input[2].OfFunctionCall)                                           // call_1
	assert.NotNil(t, input[3].OfFunctionCall)                                           // call_2
	assert.NotNil(t, input[4].OfFunctionCallOutput)                                     // result 1
	assert.NotNil(t, input[5].OfFunctionCallOutput)                                     // result 2
}

func TestConvertMessagesToResponseInput_WhitespaceOnlyAssistantText(t *testing.T) {
	// Whitespace-only text content should NOT produce an extra assistant message.
	messages := []chat.Message{
		{Role: chat.MessageRoleUser, Content: "Do something"},
		{
			Role:    chat.MessageRoleAssistant,
			Content: "   \n\t  ",
			ToolCalls: []tools.ToolCall{
				{ID: "call_1", Type: "function", Function: tools.FunctionCall{Name: "test", Arguments: "{}"}},
			},
		},
		{Role: chat.MessageRoleTool, ToolCallID: "call_1", Content: "done"},
	}

	input := convertMessagesToResponseInput(messages)

	require.Len(t, input, 3, "whitespace-only content should not produce extra message")
	assert.NotNil(t, input[0].OfMessage)
	assert.NotNil(t, input[1].OfFunctionCall)
	assert.NotNil(t, input[2].OfFunctionCallOutput)
}

func TestConvertMessagesToResponseInput_BasicFlow(t *testing.T) {
	// Verify basic conversation flow converts correctly.
	messages := []chat.Message{
		{Role: chat.MessageRoleSystem, Content: "You are helpful"},
		{Role: chat.MessageRoleUser, Content: "Hello"},
		{Role: chat.MessageRoleAssistant, Content: "Hi there!"},
		{Role: chat.MessageRoleUser, Content: "Bye"},
	}

	input := convertMessagesToResponseInput(messages)

	require.Len(t, input, 4)
	assert.NotNil(t, input[0].OfInputMessage) // system uses OfInputMessage
	assert.NotNil(t, input[1].OfMessage)      // user
	assert.NotNil(t, input[2].OfMessage)      // assistant (no tool calls)
	assert.NotNil(t, input[3].OfMessage)      // user
}
