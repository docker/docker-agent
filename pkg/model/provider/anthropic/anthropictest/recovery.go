package anthropictest

// RecoveryToolReply interleaves text, signed thinking and a malformed tool
// name. With fallback, a declined tool call and thinking precede the marker.
func RecoveryToolReply(id string, fallback bool) string {
	events := []sse{messageStart(id, "claude-opus-4-8", 10)}
	index := 0
	if fallback {
		events = append(events,
			event("content_block_start", map[string]any{"index": index, "content_block": map[string]any{"type": "thinking", "thinking": "declined plan", "signature": "sig-declined"}}),
			event("content_block_stop", map[string]any{"index": index}),
		)
		index++
		events = append(events,
			event("content_block_start", map[string]any{"index": index, "content_block": map[string]any{"type": "tool_use", "id": "t-declined", "name": "record", "input": map[string]any{}}}),
			event("content_block_stop", map[string]any{"index": index}),
		)
		index++
	}
	events = append(events, textBlock(index, "before ")...)
	index++
	if fallback {
		events = append(events,
			event("content_block_start", map[string]any{"index": index, "content_block": map[string]any{
				"type": "fallback", "from": map[string]any{"model": "claude-opus-4-8"}, "to": map[string]any{"model": "claude-sonnet-4-6"},
				"trigger": map[string]any{"type": "refusal", "category": "cyber"},
			}}),
			event("content_block_stop", map[string]any{"index": index}),
		)
		index++
	}
	events = append(events,
		event("content_block_start", map[string]any{"index": index, "content_block": map[string]any{"type": "thinking", "thinking": "plan", "signature": "sig-intact"}}),
		event("content_block_stop", map[string]any{"index": index}),
	)
	index++
	events = append(events, textBlock(index, "middle")...)
	index++
	events = append(events,
		event("content_block_start", map[string]any{"index": index, "content_block": map[string]any{"type": "tool_use", "id": "t-kept", "name": `record" path="x`, "input": map[string]any{}}}),
		event("content_block_delta", map[string]any{"index": index, "delta": map[string]any{"type": "input_json_delta", "partial_json": "{}"}}),
		event("content_block_stop", map[string]any{"index": index}),
	)
	index++
	events = append(events, textBlock(index, " after")...)
	events = append(events, messageDelta("tool_use", map[string]any{"output_tokens": 7}), event("message_stop", nil))
	return body(events...)
}
