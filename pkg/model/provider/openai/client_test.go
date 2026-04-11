package openai

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestConvertMessagesToResponseInput_OrphanedFunctionCall(t *testing.T) {
	// Simulate a conversation where an assistant made 2 tool calls but only
	// one has a result (the other was cancelled/interrupted).
	messages := []chat.Message{
		{Role: chat.MessageRoleUser, Content: "hello"},
		{
			Role: chat.MessageRoleAssistant,
			ToolCalls: []tools.ToolCall{
				{ID: "call_1", Type: "function", Function: tools.FunctionCall{Name: "tool_a", Arguments: "{}"}},
				{ID: "call_2", Type: "function", Function: tools.FunctionCall{Name: "tool_b", Arguments: "{}"}},
			},
		},
		{Role: chat.MessageRoleTool, Content: "result a", ToolCallID: "call_1"},
		// call_2 has no result — orphaned
	}

	input := (&Client{}).convertMessagesToResponseInput(t.Context(), messages)

	// Count function calls and outputs
	var callIDs, outputIDs []string
	for _, item := range input {
		if item.OfFunctionCall != nil {
			callIDs = append(callIDs, item.OfFunctionCall.CallID)
		}
		if item.OfFunctionCallOutput != nil {
			outputIDs = append(outputIDs, item.OfFunctionCallOutput.CallID)
		}
	}

	require.Len(t, callIDs, 2)
	require.Len(t, outputIDs, 2, "orphaned function call should get a placeholder output")

	assert.Contains(t, outputIDs, "call_1")
	assert.Contains(t, outputIDs, "call_2")
}

func TestConvertMessagesToResponseInput_AssistantTextWithToolCalls(t *testing.T) {
	// When an assistant message has both text content and tool calls,
	// the text must not be silently discarded.
	messages := []chat.Message{
		{Role: chat.MessageRoleUser, Content: "hello"},
		{
			Role:    chat.MessageRoleAssistant,
			Content: "Let me search that for you.",
			ToolCalls: []tools.ToolCall{
				{ID: "call_1", Type: "function", Function: tools.FunctionCall{Name: "search", Arguments: `{"q":"test"}`}},
			},
		},
		{Role: chat.MessageRoleTool, Content: "result", ToolCallID: "call_1"},
	}

	input := (&Client{}).convertMessagesToResponseInput(t.Context(), messages)

	// We expect: user message, assistant text message, function call, function call output.
	var foundAssistantText bool
	var foundFunctionCall bool
	for _, item := range input {
		if item.OfMessage != nil && item.OfMessage.Role == "assistant" {
			if item.OfMessage.Content.OfString.Valid() && item.OfMessage.Content.OfString.Value == "Let me search that for you." {
				foundAssistantText = true
			}
		}
		if item.OfFunctionCall != nil && item.OfFunctionCall.CallID == "call_1" {
			foundFunctionCall = true
		}
	}

	assert.True(t, foundFunctionCall, "function call should be present")
	assert.True(t, foundAssistantText, "assistant text content should not be discarded when tool calls are present")
}

func TestConvertMessagesToResponseInput_NoOrphans(t *testing.T) {
	// All tool calls have matching results - no placeholder needed.
	messages := []chat.Message{
		{Role: chat.MessageRoleUser, Content: "hello"},
		{
			Role: chat.MessageRoleAssistant,
			ToolCalls: []tools.ToolCall{
				{ID: "call_1", Type: "function", Function: tools.FunctionCall{Name: "tool_a", Arguments: "{}"}},
			},
		},
		{Role: chat.MessageRoleTool, Content: "result a", ToolCallID: "call_1"},
	}

	input := (&Client{}).convertMessagesToResponseInput(t.Context(), messages)

	var outputCount int
	for _, item := range input {
		if item.OfFunctionCallOutput != nil {
			outputCount++
		}
	}
	assert.Equal(t, 1, outputCount, "should not inject extra outputs when all calls have results")
}

func TestConvertMessages_MergesConsecutiveSystemMessagesForCustomProviders(t *testing.T) {
	cfg := &latest.ModelConfig{
		ProviderOpts: map[string]any{"api_type": "openai_chatcompletions"},
	}
	messages := []chat.Message{
		{Role: chat.MessageRoleSystem, Content: "You are Bob, a coding expert"},
		{Role: chat.MessageRoleSystem, Content: "## Custom Shell Tools\n\n### execute_command"},
		{Role: chat.MessageRoleSystem, Content: "<available_skills>\n  <skill>what-time-is-it</skill>\n</available_skills>"},
		{Role: chat.MessageRoleUser, Content: "what is your favourite colour?"},
	}

	result := convertMessages(t.Context(), cfg, modelsdev.ID{}, nil, messages)
	require.Len(t, result, 2)
	require.NotNil(t, result[0].OfSystem)
	assert.Contains(t, result[0].OfSystem.Content.OfString.Value, "You are Bob, a coding expert")
	assert.Contains(t, result[0].OfSystem.Content.OfString.Value, "Custom Shell Tools")
	assert.Contains(t, result[0].OfSystem.Content.OfString.Value, "available_skills")
	assert.NotNil(t, result[1].OfUser)
}

func TestConvertMessages_PreservesConsecutiveSystemMessagesForOpenAIProvider(t *testing.T) {
	messages := []chat.Message{
		{Role: chat.MessageRoleSystem, Content: "System 1"},
		{Role: chat.MessageRoleSystem, Content: "System 2"},
		{Role: chat.MessageRoleUser, Content: "hello"},
	}

	result := convertMessages(t.Context(), &latest.ModelConfig{}, modelsdev.ID{}, nil, messages)
	require.Len(t, result, 3)
	assert.NotNil(t, result[0].OfSystem)
	assert.NotNil(t, result[1].OfSystem)
	assert.NotNil(t, result[2].OfUser)
}
