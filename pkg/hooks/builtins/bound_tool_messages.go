package builtins

import (
	"slices"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/tools"
)

// BoundToolMessages repairs model-facing copies of historical results using
// the definitions saved with their calls. Unknown tools and user text are left
// untouched; neither the message slice nor its nested parts are mutated.
func BoundToolMessages(messages []chat.Message) []chat.Message {
	calls := make(map[string]tools.Tool)
	var bounded []chat.Message
	for i, msg := range messages {
		if msg.Role == chat.MessageRoleAssistant {
			for _, call := range msg.ToolCalls {
				if call.ID == "" {
					continue
				}
				delete(calls, call.ID)
				for _, definition := range msg.ToolDefinitions {
					if definition.Name == call.Function.Name {
						calls[call.ID] = definition
						break
					}
				}
			}
		}
		if msg.Role != chat.MessageRoleTool {
			continue
		}
		tool, ok := calls[msg.ToolCallID]
		if !ok || !largeResultCategories[tool.Category] {
			continue
		}
		const notice = "Omitted output remains in the original session history; narrow the tool query to retrieve the part you need."
		content := BoundToolResult(tool.Category, tool.Name, msg.Content, notice)
		var parts []chat.MessagePart
		for j, part := range msg.MultiContent {
			if part.Type != chat.MessagePartTypeText {
				continue
			}
			text := BoundToolResult(tool.Category, tool.Name, part.Text, notice)
			if text == part.Text {
				continue
			}
			if parts == nil {
				parts = slices.Clone(msg.MultiContent)
			}
			parts[j].Text = text
		}
		if content == msg.Content && parts == nil {
			continue
		}
		if bounded == nil {
			bounded = slices.Clone(messages)
		}
		bounded[i].Content = content
		if parts != nil {
			bounded[i].MultiContent = parts
		}
	}
	if bounded == nil {
		return messages
	}
	return bounded
}
