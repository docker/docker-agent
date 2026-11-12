package providerutil

import (
	"slices"

	"github.com/docker/docker-agent/pkg/chat"
)

// FinalAssistantPrefill reports whether the conversation ends with an
// assistant message that is not a tool call, i.e. a response prefill.
// Trailing system messages are ignored.
func FinalAssistantPrefill(messages []chat.Message) bool {
	for _, msg := range slices.Backward(messages) {
		switch msg.Role {
		case chat.MessageRoleSystem:
			continue
		case chat.MessageRoleAssistant:
			return len(msg.ToolCalls) == 0
		default:
			return false
		}
	}
	return false
}
