package anthropictest

// BillingHop scripts one model's server-tool iterations and refusal evidence.
// Each iteration is {input, output, cache read, cache creation}; nil Category is
// an explicit JSON null unless MissingCategory is set.
type BillingHop struct {
	Model string
	// From overrides the marker source to model the request alias echo.
	From            string
	Iterations      [][4]int64
	Category        any
	MissingCategory bool
}

// FallbackBillingReply scripts usage for a fallback chain, including a terminal
// refusal when exhausted. A single hop scripts sticky routing.
func FallbackBillingReply(id string, exhausted bool, hops ...BillingHop) string {
	events := []sse{messageStart(id, hops[0].Model, 0)}
	var iterations []any
	var last [4]int64
	for i, hop := range hops {
		typ := "message"
		if i > 0 || len(hops) == 1 {
			typ = "fallback_message"
		}
		for j, tokens := range hop.Iterations {
			rowType := typ
			if j+1 < len(hop.Iterations) {
				rowType = "message"
			}
			iterations = append(iterations, map[string]any{
				"type": rowType, "model": hop.Model, "input_tokens": tokens[0], "output_tokens": tokens[1],
				"cache_read_input_tokens": tokens[2], "cache_creation_input_tokens": tokens[3],
			})
			last = tokens
		}
		if i+1 < len(hops) {
			from := hop.From
			if from == "" {
				from = hop.Model
			}
			trigger := map[string]any{"type": "refusal"}
			if !hop.MissingCategory {
				trigger["category"] = hop.Category
			}
			events = append(events,
				event("content_block_start", map[string]any{"index": i, "content_block": map[string]any{
					"type": "fallback", "from": map[string]any{"model": from}, "to": map[string]any{"model": hops[i+1].Model}, "trigger": trigger,
				}}),
				event("content_block_stop", map[string]any{"index": i}),
			)
		}
	}
	stopReason := "end_turn"
	delta := map[string]any{"stop_reason": stopReason}
	if exhausted {
		delta["stop_reason"] = "refusal"
		lastHop := hops[len(hops)-1]
		details := map[string]any{"type": "refusal"}
		if !lastHop.MissingCategory {
			details["category"] = lastHop.Category
		}
		delta["stop_details"] = details
	}
	events = append(events, event("message_delta", map[string]any{"delta": delta, "usage": map[string]any{
		"input_tokens": last[0], "output_tokens": last[1], "cache_read_input_tokens": last[2], "cache_creation_input_tokens": last[3], "iterations": iterations,
	}}), event("message_stop", nil))
	return body(events...)
}
