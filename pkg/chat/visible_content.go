package chat

import "strings"

// VisibleAssistantContent preserves the live stream's XML tool-call suppression.
func VisibleAssistantContent(content string) string {
	visible, _, _ := strings.Cut(content, "<tool_call>")
	return visible
}
