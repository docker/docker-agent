package builtins_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestBoundToolResultAllowList(t *testing.T) {
	t.Parallel()
	for _, category := range []string{"filesystem", "shell", "mcp", "a2a", "background_jobs", "memory", "fetch", "skills", "", "custom"} {
		t.Run(category, func(t *testing.T) {
			t.Parallel()
			payload := strings.Repeat("世", 50*1024)
			got := builtins.BoundToolResult(category, "test", payload, "Output could not be saved.")
			switch category {
			case "filesystem", "shell", "mcp", "a2a", "background_jobs":
				assert.LessOrEqual(t, len(got), 50*1024)
				assert.True(t, utf8.ValidString(got))
				assert.Contains(t, got, "Output could not be saved")
				assert.Equal(t, got, builtins.BoundToolResult(category, "test", got, "Output could not be saved."))
			default:
				assert.Equal(t, payload, got)
			}
		})
	}
}

func TestBoundToolMessagesPreservesHistoryAndPairing(t *testing.T) {
	t.Parallel()
	payload := strings.Repeat("世", 50*1024)
	media := &chat.MessageImageURL{URL: "data:image/png;base64,aGVsbG8="}
	msgs := []chat.Message{
		{Role: chat.MessageRoleUser, Content: payload},
		{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{{ID: "call", Function: tools.FunctionCall{Name: "logs"}}}, ToolDefinitions: []tools.Tool{{Name: "logs", Category: "background_jobs"}}},
		{Role: chat.MessageRoleTool, ToolCallID: "call", IsError: true, Content: payload, MultiContent: []chat.MessagePart{{Type: chat.MessagePartTypeText, Text: payload}, {Type: chat.MessagePartTypeImageURL, ImageURL: media}}},
		{Role: chat.MessageRoleTool, ToolCallID: "unknown", Content: payload},
	}
	got := builtins.BoundToolMessages(msgs)
	require.Len(t, got, len(msgs))
	assert.Equal(t, msgs[0], got[0])
	assert.Equal(t, msgs[1], got[1])
	assert.Equal(t, msgs[3], got[3])
	assert.Equal(t, "call", got[2].ToolCallID)
	assert.True(t, got[2].IsError)
	assert.LessOrEqual(t, len(got[2].Content), 50*1024)
	assert.Equal(t, got[2].Content, got[2].MultiContent[0].Text)
	assert.Equal(t, media, got[2].MultiContent[1].ImageURL)
	assert.Equal(t, payload, msgs[2].Content)
	assert.Equal(t, payload, msgs[2].MultiContent[0].Text)
	assert.Equal(t, got, builtins.BoundToolMessages(got))
}

func TestBoundToolMessagesRequiresMatchingSavedDefinition(t *testing.T) {
	t.Parallel()
	for _, category := range []string{"", "memory", "custom"} {
		t.Run(category, func(t *testing.T) {
			t.Parallel()
			msgs := []chat.Message{
				{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{{ID: "call", Function: tools.FunctionCall{Name: "shell"}}}, ToolDefinitions: []tools.Tool{{Name: "shell", Category: category}}},
				{Role: chat.MessageRoleTool, ToolCallID: "call", Content: strings.Repeat("x", 100_000)},
			}
			assert.Equal(t, msgs, builtins.BoundToolMessages(msgs))
		})
	}
}

func TestBoundToolMessagesReadFileHeadAndEmptyID(t *testing.T) {
	t.Parallel()
	payload := "important beginning\n" + strings.Repeat("x", 100_000) + "discarded tail"
	for _, id := range []string{"call", ""} {
		msgs := []chat.Message{
			{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{{ID: id, Function: tools.FunctionCall{Name: "read_file"}}}, ToolDefinitions: []tools.Tool{{Name: "read_file", Category: "filesystem"}}},
			{Role: chat.MessageRoleTool, ToolCallID: id, Content: payload},
		}
		got := builtins.BoundToolMessages(msgs)
		if id == "" {
			assert.Equal(t, msgs, got)
			continue
		}
		assert.Contains(t, got[1].Content, "important beginning")
		assert.NotContains(t, got[1].Content, "discarded tail")
		assert.LessOrEqual(t, len(got[1].Content), 50*1024)
	}
}

func TestBoundToolResultBoundaries(t *testing.T) {
	t.Parallel()
	for _, size := range []int{0, 50*1024 - 1, 50 * 1024, 50*1024 + 1} {
		payload := strings.Repeat("x", size)
		got := builtins.BoundToolResult("shell", "shell", payload, "Not saved.")
		assert.LessOrEqual(t, len(got), 50*1024)
		if size <= 50*1024 {
			assert.Equal(t, payload, got)
		} else {
			assert.Contains(t, got, "Not saved.")
		}
	}
}
