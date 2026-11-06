package providerutil

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestFinalAssistantPrefill(t *testing.T) {
	t.Parallel()

	user := chat.Message{Role: chat.MessageRoleUser, Content: "hi"}
	sys := chat.Message{Role: chat.MessageRoleSystem, Content: "be brief"}
	prefill := chat.Message{Role: chat.MessageRoleAssistant, Content: "Sure:"}
	toolCall := chat.Message{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{{ID: "t1"}}}
	toolResult := chat.Message{Role: chat.MessageRoleTool, ToolCallID: "t1", Content: "ok"}

	assert.True(t, FinalAssistantPrefill([]chat.Message{user, prefill}))
	assert.True(t, FinalAssistantPrefill([]chat.Message{user, prefill, sys}), "trailing system messages are ignored")
	assert.False(t, FinalAssistantPrefill([]chat.Message{user, toolCall, toolResult}))
	assert.False(t, FinalAssistantPrefill([]chat.Message{user, toolCall}))
	assert.False(t, FinalAssistantPrefill([]chat.Message{user}))
	assert.False(t, FinalAssistantPrefill(nil))
}
