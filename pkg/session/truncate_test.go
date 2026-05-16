package session

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestApproximateTokens(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{"empty", "", 0},
		{"shorter than divisor", "abc", 0},
		{"exact divisor", "abcd", 1},
		{"twice divisor", "abcdefgh", 2},
		{"rounds down", "abcdefg", 1},
		{"large input", strings.Repeat("x", 4000), 1000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := approximateTokens(tt.in); got != tt.want {
				t.Errorf("approximateTokens(%d chars) = %d, want %d", len(tt.in), got, tt.want)
			}
		})
	}
}

func TestTruncateOldToolContent(t *testing.T) {
	t.Run("keeps recent tool content within budget", func(t *testing.T) {
		messages := []chat.Message{
			{Role: chat.MessageRoleUser, Content: "hello"},
			{
				Role: chat.MessageRoleAssistant,
				ToolCalls: []tools.ToolCall{
					{ID: "1", Function: tools.FunctionCall{Name: "test", Arguments: `{"key":"value"}`}},
				},
			},
			{Role: chat.MessageRoleTool, ToolCallID: "1", Content: "result"},
		}

		result := truncateOldToolContent(messages, 1000)

		assert.JSONEq(t, `{"key":"value"}`, result[1].ToolCalls[0].Function.Arguments)
		assert.Equal(t, "result", result[2].Content)
	})

	t.Run("truncates oldest tool content when budget exceeded", func(t *testing.T) {
		oldArgs := strings.Repeat("a", 400)   // 100 tokens
		oldResult := strings.Repeat("b", 400) // 100 tokens
		newArgs := strings.Repeat("c", 200)   // 50 tokens
		newResult := strings.Repeat("d", 200) // 50 tokens

		messages := []chat.Message{
			{Role: chat.MessageRoleUser, Content: "first"},
			{
				Role: chat.MessageRoleAssistant,
				ToolCalls: []tools.ToolCall{
					{ID: "old", Function: tools.FunctionCall{Name: "test", Arguments: oldArgs}},
				},
			},
			{Role: chat.MessageRoleTool, ToolCallID: "old", Content: oldResult},
			{Role: chat.MessageRoleUser, Content: "second"},
			{
				Role: chat.MessageRoleAssistant,
				ToolCalls: []tools.ToolCall{
					{ID: "new", Function: tools.FunctionCall{Name: "test", Arguments: newArgs}},
				},
			},
			{Role: chat.MessageRoleTool, ToolCallID: "new", Content: newResult},
		}

		// Budget of 60 tokens: new result (50 tokens) fits, old result gets truncated
		result := truncateOldToolContent(messages, 60)

		// New result should be preserved, old result should be truncated
		assert.Equal(t, newResult, result[5].Content)
		assert.Equal(t, toolContentPlaceholder, result[2].Content)
	})

	t.Run("does not modify non-tool messages", func(t *testing.T) {
		messages := []chat.Message{
			{Role: chat.MessageRoleUser, Content: strings.Repeat("x", 1000)},
			{Role: chat.MessageRoleAssistant, Content: strings.Repeat("y", 1000)},
			{Role: chat.MessageRoleSystem, Content: strings.Repeat("z", 1000)},
		}

		result := truncateOldToolContent(messages, 10)

		assert.Equal(t, messages[0].Content, result[0].Content)
		assert.Equal(t, messages[1].Content, result[1].Content)
		assert.Equal(t, messages[2].Content, result[2].Content)
	})

	t.Run("returns original messages when maxTokens is zero", func(t *testing.T) {
		messages := []chat.Message{
			{
				Role: chat.MessageRoleAssistant,
				ToolCalls: []tools.ToolCall{
					{ID: "1", Function: tools.FunctionCall{Name: "test", Arguments: "args"}},
				},
			},
			{Role: chat.MessageRoleTool, ToolCallID: "1", Content: "result"},
		}

		result := truncateOldToolContent(messages, 0)

		assert.Equal(t, messages, result)
	})

	t.Run("returns original messages when maxTokens is negative", func(t *testing.T) {
		messages := []chat.Message{
			{
				Role: chat.MessageRoleAssistant,
				ToolCalls: []tools.ToolCall{
					{ID: "1", Function: tools.FunctionCall{Name: "test", Arguments: "args"}},
				},
			},
			{Role: chat.MessageRoleTool, ToolCallID: "1", Content: "result"},
		}

		result := truncateOldToolContent(messages, -10)

		assert.Equal(t, messages, result)
	})

	t.Run("does not modify original slice", func(t *testing.T) {
		originalContent := strings.Repeat("y", 400)
		messages := []chat.Message{
			{
				Role: chat.MessageRoleAssistant,
				ToolCalls: []tools.ToolCall{
					{ID: "1", Function: tools.FunctionCall{Name: "test", Arguments: `{"key":"value"}`}},
				},
			},
			{Role: chat.MessageRoleTool, ToolCallID: "1", Content: originalContent},
		}

		result := truncateOldToolContent(messages, 10)

		// Result should have truncated tool content
		assert.Equal(t, toolContentPlaceholder, result[1].Content)

		// Original should be unchanged
		assert.Equal(t, originalContent, messages[1].Content)
	})

	t.Run("handles empty messages slice", func(t *testing.T) {
		result := truncateOldToolContent(nil, 1000)
		assert.Nil(t, result)

		result = truncateOldToolContent([]chat.Message{}, 1000)
		require.NotNil(t, result)
		assert.Empty(t, result)
	})
}
