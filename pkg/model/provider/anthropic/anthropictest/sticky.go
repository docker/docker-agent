package anthropictest

import "strings"

// Truncate ends a reply before the first occurrence of the named SSE event.
func Truncate(reply, beforeEvent string) string {
	if before, _, ok := strings.Cut(reply, "event: "+beforeEvent+"\n"); ok {
		return before
	}
	return reply
}

// StickyTruncatedReply ends after text, before message_delta can confirm the serving model.
func StickyTruncatedReply(id, model, text string) string {
	return Truncate(StickyReply(id, model, text), "message_delta")
}

// StickyMessageUsageReply reports sticky routing using a message iteration, without a marker.
func StickyMessageUsageReply(id, model, text string) string {
	events := append([]sse{messageStart(id, model, 20)}, textBlock(0, text)...)
	events = append(events, messageDelta("end_turn", map[string]any{
		"output_tokens": 6,
		"iterations":    []any{map[string]any{"type": "message", "model": model, "input_tokens": 20, "output_tokens": 6}},
	}), event("message_stop", nil))
	return body(events...)
}
