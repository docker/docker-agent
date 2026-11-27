package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/docker/docker-agent/pkg/model/provider/internal/httpx"
)

// fakeDecoder feeds a fixed sequence of SSE events to EventStream.
type fakeDecoder struct {
	events []httpx.SSEEvent
	i      int
	closed bool
}

func (d *fakeDecoder) Next() bool {
	if d.i >= len(d.events) {
		return false
	}
	d.i++
	return true
}

func (d *fakeDecoder) Event() httpx.SSEEvent { return d.events[d.i-1] }
func (d *fakeDecoder) Close() error          { d.closed = true; return nil }
func (d *fakeDecoder) Err() error            { return nil }

func sseEvent(t string, payload any) httpx.SSEEvent {
	data, _ := json.Marshal(payload)
	return httpx.SSEEvent{Event: t, Data: data}
}

// TestParallelToolCallIDsAreNotCrossWired reproduces a bug where the Anthropic
// stream adapter loses track of which tool_use block a given input_json delta
// belongs to when two or more tool calls stream in parallel.
//
// Anthropic's streaming protocol emits a content_block_start for each
// tool_use block (each with its own block index and its own tool ID), then
// emits content_block_delta events of type input_json_delta carrying partial
// JSON for each block. Every event carries the block's index. The adapter
// must use that index to route partial JSON back to the correct tool call.
//
// The adapter used to store the most recently seen tool ID in a single
// field, which was overwritten when a second tool_use block started.
// Subsequent input_json deltas for the FIRST block then carried the SECOND
// block's ID, and the runtime accumulator (keyed by tool call ID in
// pkg/runtime/streaming.go) concatenated both calls' argument fragments
// into the same buffer, producing malformed JSON. This test guards the fix:
// routing by block index keeps each tool call's arguments in its own buffer.
func TestParallelToolCallIDsAreNotCrossWired(t *testing.T) {
	t.Parallel()
	// Event sequence: two parallel tool_use blocks with interleaved
	// input_json_delta events. This mirrors what Anthropic emits when the
	// model issues parallel tool calls.
	events := []httpx.SSEEvent{
		// message_start (minimal — we only care about content blocks below)
		sseEvent("message_start", map[string]any{
			"type":    "message_start",
			"message": map[string]any{"id": "msg_test", "model": "claude-test", "role": "assistant", "type": "message"},
		}),

		// content_block_start, index 0: tool A (memory_refresh_complete)
		sseEvent("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": 0,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    "toolu_AAA",
				"name":  "memory_refresh_complete",
				"input": map[string]any{},
			},
		}),

		// content_block_start, index 1: tool B (memory_learning_add)
		sseEvent("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": 1,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    "toolu_BBB",
				"name":  "memory_learning_add",
				"input": map[string]any{},
			},
		}),

		// Interleaved input_json_delta events. Each carries its block index.
		// Tool A args: {"refresh_id":"abc-def"}
		// Tool B args: {"category":"tool_failure","summary":"x"}
		sseEvent("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": `{"refresh_id":"abc-`},
		}),
		sseEvent("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": 1,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": `{"category":"tool_failure",`},
		}),
		sseEvent("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": `def"}`},
		}),
		sseEvent("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": 1,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": `"summary":"x"}`},
		}),

		sseEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}),
		sseEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": 1}),
		sseEvent("message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": "tool_use"},
		}),
		sseEvent("message_stop", map[string]any{"type": "message_stop"}),
	}

	stream := &EventStream{sse: &fakeDecoder{events: events}}
	adapter := (&Client{}).newStreamAdapter(stream, false)

	// Replicate the runtime's tool-call accumulator (pkg/runtime/streaming.go).
	// It keys by ToolCall.ID and concatenates Arguments fragments. This is the
	// downstream layer that turns malformed concatenation into a JSON parse
	// error when the tool is invoked.
	argsByID := map[string]*strings.Builder{}
	nameByID := map[string]string{}

	for {
		resp, err := adapter.Recv()
		if err != nil {
			break
		}
		if len(resp.Choices) == 0 {
			continue
		}
		for _, tc := range resp.Choices[0].Delta.ToolCalls {
			if _, ok := argsByID[tc.ID]; !ok {
				argsByID[tc.ID] = &strings.Builder{}
			}
			if tc.Function.Name != "" {
				nameByID[tc.ID] = tc.Function.Name
			}
			argsByID[tc.ID].WriteString(tc.Function.Arguments)
		}
	}

	// Expected behaviour: tool A and tool B each get exactly their own args,
	// and both parse as valid JSON.
	expectedA := `{"refresh_id":"abc-def"}`
	expectedB := `{"category":"tool_failure","summary":"x"}`

	gotA := argsByID["toolu_AAA"].String()
	gotB := argsByID["toolu_BBB"].String()

	t.Logf("toolu_AAA name=%q args=%q", nameByID["toolu_AAA"], gotA)
	t.Logf("toolu_BBB name=%q args=%q", nameByID["toolu_BBB"], gotB)

	if gotA != expectedA {
		t.Errorf("tool A (toolu_AAA, memory_refresh_complete) args wrong\n  want: %s\n   got: %s", expectedA, gotA)
	}
	if gotB != expectedB {
		t.Errorf("tool B (toolu_BBB, memory_learning_add) args wrong\n  want: %s\n   got: %s", expectedB, gotB)
	}

	// As a sharper assertion: both buffers must individually parse as JSON.
	// With the bug, one or both fail with the exact Go errors observed in
	// production ("invalid character ... looking for beginning of value" or
	// "invalid character ... after object key:value pair").
	var sink any
	if err := json.Unmarshal([]byte(gotA), &sink); err != nil {
		t.Errorf("tool A args failed to parse as JSON: %v\n  buffer was: %s", err, gotA)
	}
	if err := json.Unmarshal([]byte(gotB), &sink); err != nil {
		t.Errorf("tool B args failed to parse as JSON: %v\n  buffer was: %s", err, gotB)
	}
}

// TestBetaParallelToolCallIDsAreNotCrossWired is the same scenario but for
// the Beta stream adapter. The bug and fix are identical.
func TestBetaParallelToolCallIDsAreNotCrossWired(t *testing.T) {
	t.Parallel()
	events := []httpx.SSEEvent{
		sseEvent("message_start", map[string]any{
			"type":    "message_start",
			"message": map[string]any{"id": "msg_test", "model": "claude-test", "role": "assistant", "type": "message"},
		}),
		sseEvent("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": 0,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    "toolu_AAA",
				"name":  "memory_refresh_complete",
				"input": map[string]any{},
			},
		}),
		sseEvent("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": 1,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    "toolu_BBB",
				"name":  "memory_learning_add",
				"input": map[string]any{},
			},
		}),
		sseEvent("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": `{"refresh_id":"abc-`},
		}),
		sseEvent("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": 1,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": `{"category":"tool_failure",`},
		}),
		sseEvent("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": `def"}`},
		}),
		sseEvent("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": 1,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": `"summary":"x"}`},
		}),
		sseEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}),
		sseEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": 1}),
		sseEvent("message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": "tool_use"},
		}),
		sseEvent("message_stop", map[string]any{"type": "message_stop"}),
	}

	stream := &EventStream{sse: &fakeDecoder{events: events}}
	adapter := (&Client{}).newBetaStreamAdapter(stream, false)

	argsByID := map[string]*strings.Builder{}
	for {
		resp, err := adapter.Recv()
		if err != nil {
			break
		}
		if len(resp.Choices) == 0 {
			continue
		}
		for _, tc := range resp.Choices[0].Delta.ToolCalls {
			if _, ok := argsByID[tc.ID]; !ok {
				argsByID[tc.ID] = &strings.Builder{}
			}
			argsByID[tc.ID].WriteString(tc.Function.Arguments)
		}
	}

	gotA := argsByID["toolu_AAA"].String()
	gotB := argsByID["toolu_BBB"].String()
	expectedA := `{"refresh_id":"abc-def"}`
	expectedB := `{"category":"tool_failure","summary":"x"}`
	if gotA != expectedA {
		t.Errorf("[beta] tool A args wrong\n  want: %s\n   got: %s", expectedA, gotA)
	}
	if gotB != expectedB {
		t.Errorf("[beta] tool B args wrong\n  want: %s\n   got: %s", expectedB, gotB)
	}
	var sink any
	if err := json.Unmarshal([]byte(gotA), &sink); err != nil {
		t.Errorf("[beta] tool A args failed to parse: %v\n  buffer was: %s", err, gotA)
	}
	if err := json.Unmarshal([]byte(gotB), &sink); err != nil {
		t.Errorf("[beta] tool B args failed to parse: %v\n  buffer was: %s", err, gotB)
	}
}
