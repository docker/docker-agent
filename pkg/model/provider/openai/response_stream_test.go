package openai

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3/responses"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
)

// fakeEventStream replays pre-decoded Responses API events. It implements
// responseEventStream so the adapter can be tested without a transport.
type fakeEventStream struct {
	events []responses.ResponseStreamEventUnion
	pos    int
}

func (s *fakeEventStream) Next() bool {
	if s.pos >= len(s.events) {
		return false
	}
	s.pos++
	return true
}

func (s *fakeEventStream) Current() responses.ResponseStreamEventUnion { return s.events[s.pos-1] }
func (s *fakeEventStream) Err() error                                  { return nil }
func (s *fakeEventStream) Close() error                                { return nil }

// decodeEvents converts wire-format JSON events into SDK unions, exactly as
// the SSE and WebSocket transports do.
func decodeEvents(t *testing.T, raw []map[string]any) []responses.ResponseStreamEventUnion {
	t.Helper()
	events := make([]responses.ResponseStreamEventUnion, len(raw))
	for i, m := range raw {
		data, err := json.Marshal(m)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(data, &events[i]))
	}
	return events
}

func toolCallsCompletedEvent(id string) map[string]any {
	return map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id": id,
			"output": []any{
				map[string]any{"type": "function_call"},
			},
			"usage": map[string]any{
				"input_tokens":          8,
				"output_tokens":         12,
				"total_tokens":          20,
				"input_tokens_details":  map[string]any{"cached_tokens": 0},
				"output_tokens_details": map[string]any{"reasoning_tokens": 0},
			},
		},
	}
}

// accumulatedToolCall mirrors how the runtime merges tool-call deltas: names
// overwrite, argument bytes append. Duplicated argument emissions therefore
// show up as doubled JSON.
type accumulatedToolCall struct {
	name string
	args string
}

// drainToolCalls runs the adapter to EOF and accumulates tool-call deltas per
// call ID. It returns the merged calls and the last finish reason seen.
func drainToolCalls(t *testing.T, adapter *ResponseStreamAdapter) (map[string]accumulatedToolCall, chat.FinishReason) {
	t.Helper()
	calls := make(map[string]accumulatedToolCall)
	var finishReason chat.FinishReason
	for {
		resp, err := adapter.Recv()
		if errors.Is(err, io.EOF) {
			return calls, finishReason
		}
		require.NoError(t, err)
		for _, choice := range resp.Choices {
			if choice.FinishReason != "" {
				finishReason = choice.FinishReason
			}
			for _, tc := range choice.Delta.ToolCalls {
				call := calls[tc.ID]
				if tc.Function.Name != "" {
					call.name = tc.Function.Name
				}
				call.args += tc.Function.Arguments
				calls[tc.ID] = call
			}
		}
	}
}

// drainContent runs the adapter to EOF and concatenates text deltas, exactly
// as the runtime accumulates assistant content. Duplicated emissions
// therefore show up as doubled text.
func drainContent(t *testing.T, adapter *ResponseStreamAdapter) (string, chat.FinishReason) {
	t.Helper()
	var content strings.Builder
	var finishReason chat.FinishReason
	for {
		resp, err := adapter.Recv()
		if errors.Is(err, io.EOF) {
			return content.String(), finishReason
		}
		require.NoError(t, err)
		for _, choice := range resp.Choices {
			if choice.FinishReason != "" {
				finishReason = choice.FinishReason
			}
			content.WriteString(choice.Delta.Content)
		}
	}
}

func TestResponseStream_FunctionCallName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		topName  string
		itemName string
		wantName string
	}{
		{name: "item name", itemName: "shell", wantName: "shell"},
		{name: "top-level name", topName: `"shell"`, wantName: "shell"},
		{name: "top-level name takes precedence", topName: `"shell"`, itemName: "other", wantName: "shell"},
		{name: "empty top-level name", topName: `""`, itemName: "shell", wantName: "shell"},
		{name: "null top-level name", topName: `null`, itemName: "shell", wantName: "shell"},
		{name: "numeric top-level name", topName: `42`, itemName: "shell", wantName: "shell"},
		{name: "object top-level name", topName: `{"name":"other"}`, itemName: "shell", wantName: "shell"},
		{name: "array top-level name", topName: `["other"]`, itemName: "shell", wantName: "shell"},
		{name: "escaped top-level name", topName: `"sh\u0065ll"`, wantName: "shell"},
		{name: "missing names"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			item := map[string]any{
				"type":    "function_call",
				"id":      "fc_1",
				"call_id": "call_1",
			}
			if tt.itemName != "" {
				item["name"] = tt.itemName
			}
			added := map[string]any{
				"type": "response.output_item.added",
				"item": item,
			}
			if tt.topName != "" {
				added["name"] = json.RawMessage(tt.topName)
			}

			const args = `{"cmd":"ls"}`
			events := decodeEvents(t, []map[string]any{
				{
					"type":    "response.function_call_arguments.delta",
					"item_id": "fc_1",
					"delta":   args,
				},
				added,
			})
			adapter := newResponseStreamAdapter(&fakeEventStream{events: events}, true)

			resp, err := adapter.Recv()
			require.NoError(t, err)
			assert.Empty(t, resp.Choices)

			resp, err = adapter.Recv()
			require.NoError(t, err)
			if tt.wantName == "" {
				assert.Empty(t, resp.Choices)
			} else {
				require.Len(t, resp.Choices, 1)
				require.Len(t, resp.Choices[0].Delta.ToolCalls, 1)
				call := resp.Choices[0].Delta.ToolCalls[0]
				assert.Equal(t, "call_1", call.ID)
				assert.Equal(t, "function", string(call.Type))
				assert.Equal(t, tt.wantName, call.Function.Name)
				assert.JSONEq(t, args, call.Function.Arguments)
			}

			_, err = adapter.Recv()
			assert.ErrorIs(t, err, io.EOF)
		})
	}
}

// TestResponseStream_ArgumentsOnlyInDoneEvent is a regression test for
// https://github.com/docker/docker-agent/issues/3818.
//
// Some Responses API implementations (e.g. GitHub Copilot) announce the
// function call in response.output_item.added, send no
// response.function_call_arguments.delta events, and deliver the complete
// JSON arguments only in response.function_call_arguments.done plus the
// response.output_item.done snapshot. The adapter must emit those final
// arguments exactly once.
func TestResponseStream_ArgumentsOnlyInDoneEvent(t *testing.T) {
	t.Parallel()

	const args = `{"cmd":"ls -la"}`
	events := decodeEvents(t, []map[string]any{
		{
			"type":    "response.output_item.added",
			"item_id": "fc_1",
			"item": map[string]any{
				"type":    "function_call",
				"id":      "fc_1",
				"call_id": "call_1",
				"name":    "shell",
			},
		},
		{
			"type":      "response.function_call_arguments.done",
			"item_id":   "fc_1",
			"name":      "shell",
			"arguments": args,
		},
		{
			"type":    "response.output_item.done",
			"item_id": "fc_1",
			"item": map[string]any{
				"type":      "function_call",
				"id":        "fc_1",
				"call_id":   "call_1",
				"name":      "shell",
				"arguments": args,
				"status":    "completed",
			},
		},
		toolCallsCompletedEvent("resp_args_done_only"),
	})

	adapter := newResponseStreamAdapter(&fakeEventStream{events: events}, true)

	// output_item.added → tool call announced with name, no arguments yet.
	resp, err := adapter.Recv()
	require.NoError(t, err)
	require.Len(t, resp.Choices, 1)
	require.Len(t, resp.Choices[0].Delta.ToolCalls, 1)
	assert.Equal(t, "call_1", resp.Choices[0].Delta.ToolCalls[0].ID)
	assert.Equal(t, "shell", resp.Choices[0].Delta.ToolCalls[0].Function.Name)
	assert.Empty(t, resp.Choices[0].Delta.ToolCalls[0].Function.Arguments)

	// function_call_arguments.done → no deltas were streamed, so the
	// complete arguments must be emitted here.
	resp, err = adapter.Recv()
	require.NoError(t, err)
	require.Len(t, resp.Choices, 1)
	require.Len(t, resp.Choices[0].Delta.ToolCalls, 1)
	assert.Equal(t, "call_1", resp.Choices[0].Delta.ToolCalls[0].ID)
	assert.JSONEq(t, args, resp.Choices[0].Delta.ToolCalls[0].Function.Arguments)

	// output_item.done → arguments already emitted, the snapshot must not
	// duplicate them.
	resp, err = adapter.Recv()
	require.NoError(t, err)
	assert.Empty(t, resp.Choices)

	resp, err = adapter.Recv()
	require.NoError(t, err)
	require.Len(t, resp.Choices, 1)
	assert.Equal(t, chat.FinishReasonToolCalls, resp.Choices[0].FinishReason)

	_, err = adapter.Recv()
	assert.ErrorIs(t, err, io.EOF)
}

// TestResponseStream_LateDeltaAfterArgumentsDoneIgnored is a regression test
// for the sequence added -> arguments.done (complete args, no prior deltas)
// -> late arguments.delta -> output_item.done snapshot. The final arguments
// were already emitted by arguments.done, so the late delta must be dropped:
// appending it would corrupt the JSON.
func TestResponseStream_LateDeltaAfterArgumentsDoneIgnored(t *testing.T) {
	t.Parallel()

	const args = `{"cmd":"ls -la"}`
	events := decodeEvents(t, []map[string]any{
		{
			"type":    "response.output_item.added",
			"item_id": "fc_1",
			"item": map[string]any{
				"type":    "function_call",
				"id":      "fc_1",
				"call_id": "call_1",
				"name":    "shell",
			},
		},
		{
			"type":      "response.function_call_arguments.done",
			"item_id":   "fc_1",
			"name":      "shell",
			"arguments": args,
		},
		{
			"type":    "response.function_call_arguments.delta",
			"item_id": "fc_1",
			"delta":   "EXTRA",
		},
		{
			"type":    "response.output_item.done",
			"item_id": "fc_1",
			"item": map[string]any{
				"type":      "function_call",
				"id":        "fc_1",
				"call_id":   "call_1",
				"name":      "shell",
				"arguments": args,
				"status":    "completed",
			},
		},
		toolCallsCompletedEvent("resp_late_delta_after_done"),
	})

	adapter := newResponseStreamAdapter(&fakeEventStream{events: events}, true)

	// output_item.added → tool call announced with name, no arguments yet.
	resp, err := adapter.Recv()
	require.NoError(t, err)
	require.Len(t, resp.Choices, 1)
	require.Len(t, resp.Choices[0].Delta.ToolCalls, 1)
	assert.Equal(t, "shell", resp.Choices[0].Delta.ToolCalls[0].Function.Name)
	assert.Empty(t, resp.Choices[0].Delta.ToolCalls[0].Function.Arguments)

	// function_call_arguments.done → the complete arguments are emitted once.
	resp, err = adapter.Recv()
	require.NoError(t, err)
	require.Len(t, resp.Choices, 1)
	require.Len(t, resp.Choices[0].Delta.ToolCalls, 1)
	assert.Equal(t, "call_1", resp.Choices[0].Delta.ToolCalls[0].ID)
	assert.JSONEq(t, args, resp.Choices[0].Delta.ToolCalls[0].Function.Arguments)

	// Late arguments.delta → must be ignored, not appended.
	resp, err = adapter.Recv()
	require.NoError(t, err)
	assert.Empty(t, resp.Choices)

	// output_item.done → arguments already emitted, the snapshot must not
	// duplicate them.
	resp, err = adapter.Recv()
	require.NoError(t, err)
	assert.Empty(t, resp.Choices)

	resp, err = adapter.Recv()
	require.NoError(t, err)
	require.Len(t, resp.Choices, 1)
	assert.Equal(t, chat.FinishReasonToolCalls, resp.Choices[0].FinishReason)

	_, err = adapter.Recv()
	assert.ErrorIs(t, err, io.EOF)
}

// TestResponseStream_ArgumentsOnlyInOutputItemDoneSnapshot covers the
// fallback where neither argument deltas nor an arguments.done payload were
// received and only the output_item.done item snapshot carries the final
// arguments.
func TestResponseStream_ArgumentsOnlyInOutputItemDoneSnapshot(t *testing.T) {
	t.Parallel()

	const args = `{"path":"main.go"}`
	events := decodeEvents(t, []map[string]any{
		{
			"type":    "response.output_item.added",
			"item_id": "fc_1",
			"item": map[string]any{
				"type":    "function_call",
				"id":      "fc_1",
				"call_id": "call_1",
				"name":    "read_file",
			},
		},
		{
			"type":    "response.function_call_arguments.done",
			"item_id": "fc_1",
		},
		{
			"type":    "response.output_item.done",
			"item_id": "fc_1",
			"item": map[string]any{
				"type":      "function_call",
				"id":        "fc_1",
				"call_id":   "call_1",
				"name":      "read_file",
				"arguments": args,
				"status":    "completed",
			},
		},
		toolCallsCompletedEvent("resp_snapshot_only"),
	})

	adapter := newResponseStreamAdapter(&fakeEventStream{events: events}, true)

	resp, err := adapter.Recv()
	require.NoError(t, err)
	require.Len(t, resp.Choices, 1)
	require.Len(t, resp.Choices[0].Delta.ToolCalls, 1)
	assert.Equal(t, "read_file", resp.Choices[0].Delta.ToolCalls[0].Function.Name)
	assert.Empty(t, resp.Choices[0].Delta.ToolCalls[0].Function.Arguments)

	// arguments.done carries no arguments → nothing to emit.
	resp, err = adapter.Recv()
	require.NoError(t, err)
	assert.Empty(t, resp.Choices)

	// output_item.done → the snapshot is the only source of the arguments.
	resp, err = adapter.Recv()
	require.NoError(t, err)
	require.Len(t, resp.Choices, 1)
	require.Len(t, resp.Choices[0].Delta.ToolCalls, 1)
	assert.Equal(t, "call_1", resp.Choices[0].Delta.ToolCalls[0].ID)
	assert.JSONEq(t, args, resp.Choices[0].Delta.ToolCalls[0].Function.Arguments)

	resp, err = adapter.Recv()
	require.NoError(t, err)
	require.Len(t, resp.Choices, 1)
	assert.Equal(t, chat.FinishReasonToolCalls, resp.Choices[0].FinishReason)

	_, err = adapter.Recv()
	assert.ErrorIs(t, err, io.EOF)
}

// TestResponseStream_StreamedDeltasNotDuplicatedByDoneEvents pins down that
// the done events stay redundant when arguments were streamed normally: the
// OpenAI API repeats the full arguments in both function_call_arguments.done
// and the output_item.done snapshot, and neither may be re-emitted.
func TestResponseStream_StreamedDeltasNotDuplicatedByDoneEvents(t *testing.T) {
	t.Parallel()

	const args = `{"cmd":"go test ./..."}`
	events := decodeEvents(t, []map[string]any{
		{
			"type":    "response.output_item.added",
			"item_id": "fc_1",
			"item": map[string]any{
				"type":    "function_call",
				"id":      "fc_1",
				"call_id": "call_1",
				"name":    "shell",
			},
		},
		{
			"type":    "response.function_call_arguments.delta",
			"item_id": "fc_1",
			"delta":   `{"cmd":"go test`,
		},
		{
			"type":    "response.function_call_arguments.delta",
			"item_id": "fc_1",
			"delta":   ` ./..."}`,
		},
		{
			"type":      "response.function_call_arguments.done",
			"item_id":   "fc_1",
			"name":      "shell",
			"arguments": args,
		},
		{
			"type":    "response.output_item.done",
			"item_id": "fc_1",
			"item": map[string]any{
				"type":      "function_call",
				"id":        "fc_1",
				"call_id":   "call_1",
				"name":      "shell",
				"arguments": args,
				"status":    "completed",
			},
		},
		toolCallsCompletedEvent("resp_streamed_deltas"),
	})

	adapter := newResponseStreamAdapter(&fakeEventStream{events: events}, true)

	calls, finishReason := drainToolCalls(t, adapter)
	require.Len(t, calls, 1)
	assert.Equal(t, "shell", calls["call_1"].name)
	assert.JSONEq(t, args, calls["call_1"].args)
	assert.Equal(t, chat.FinishReasonToolCalls, finishReason)
}

// TestResponseStream_LateDeltaAfterStreamedDeltasAndDoneIgnored guards the
// normal sequence added -> deltas -> arguments.done followed by a late
// arguments.delta. The non-empty done payload is by definition the complete
// final arguments, so the late delta must be dropped even though deltas were
// already emitted: neither the done event may duplicate the arguments nor
// the late delta corrupt them.
func TestResponseStream_LateDeltaAfterStreamedDeltasAndDoneIgnored(t *testing.T) {
	t.Parallel()

	const args = `{"cmd":"go test ./..."}`
	events := decodeEvents(t, []map[string]any{
		{
			"type":    "response.output_item.added",
			"item_id": "fc_1",
			"item": map[string]any{
				"type":    "function_call",
				"id":      "fc_1",
				"call_id": "call_1",
				"name":    "shell",
			},
		},
		{
			"type":    "response.function_call_arguments.delta",
			"item_id": "fc_1",
			"delta":   `{"cmd":"go test`,
		},
		{
			"type":    "response.function_call_arguments.delta",
			"item_id": "fc_1",
			"delta":   ` ./..."}`,
		},
		{
			"type":      "response.function_call_arguments.done",
			"item_id":   "fc_1",
			"name":      "shell",
			"arguments": args,
		},
		{
			"type":    "response.function_call_arguments.delta",
			"item_id": "fc_1",
			"delta":   "EXTRA",
		},
		{
			"type":    "response.output_item.done",
			"item_id": "fc_1",
			"item": map[string]any{
				"type":      "function_call",
				"id":        "fc_1",
				"call_id":   "call_1",
				"name":      "shell",
				"arguments": args,
				"status":    "completed",
			},
		},
		toolCallsCompletedEvent("resp_late_delta_after_streamed_done"),
	})

	adapter := newResponseStreamAdapter(&fakeEventStream{events: events}, true)

	calls, finishReason := drainToolCalls(t, adapter)
	require.Len(t, calls, 1)
	assert.Equal(t, "shell", calls["call_1"].name)
	assert.JSONEq(t, args, calls["call_1"].args)
	assert.Equal(t, chat.FinishReasonToolCalls, finishReason)
}

// TestResponseStream_InterleavedCallsMixedArgumentDelivery verifies per-item
// tracking with two interleaved calls: one streams argument deltas, the other
// only delivers its arguments in the done event.
func TestResponseStream_InterleavedCallsMixedArgumentDelivery(t *testing.T) {
	t.Parallel()

	const argsA = `{"cmd":"ls"}`
	const argsB = `{"path":"go.mod"}`
	events := decodeEvents(t, []map[string]any{
		{
			"type":    "response.output_item.added",
			"item_id": "fc_a",
			"item": map[string]any{
				"type":    "function_call",
				"id":      "fc_a",
				"call_id": "call_a",
				"name":    "shell",
			},
		},
		{
			"type":    "response.output_item.added",
			"item_id": "fc_b",
			"item": map[string]any{
				"type":    "function_call",
				"id":      "fc_b",
				"call_id": "call_b",
				"name":    "read_file",
			},
		},
		{
			"type":    "response.function_call_arguments.delta",
			"item_id": "fc_a",
			"delta":   argsA,
		},
		{
			"type":      "response.function_call_arguments.done",
			"item_id":   "fc_b",
			"name":      "read_file",
			"arguments": argsB,
		},
		{
			"type":      "response.function_call_arguments.done",
			"item_id":   "fc_a",
			"name":      "shell",
			"arguments": argsA,
		},
		{
			"type":    "response.output_item.done",
			"item_id": "fc_a",
			"item": map[string]any{
				"type":      "function_call",
				"id":        "fc_a",
				"call_id":   "call_a",
				"name":      "shell",
				"arguments": argsA,
				"status":    "completed",
			},
		},
		{
			"type":    "response.output_item.done",
			"item_id": "fc_b",
			"item": map[string]any{
				"type":      "function_call",
				"id":        "fc_b",
				"call_id":   "call_b",
				"name":      "read_file",
				"arguments": argsB,
				"status":    "completed",
			},
		},
		toolCallsCompletedEvent("resp_interleaved"),
	})

	adapter := newResponseStreamAdapter(&fakeEventStream{events: events}, true)

	calls, finishReason := drainToolCalls(t, adapter)
	require.Len(t, calls, 2)
	assert.Equal(t, "shell", calls["call_a"].name)
	assert.JSONEq(t, argsA, calls["call_a"].args)
	assert.Equal(t, "read_file", calls["call_b"].name)
	assert.JSONEq(t, argsB, calls["call_b"].args)
	assert.Equal(t, chat.FinishReasonToolCalls, finishReason)
}

// TestResponseStream_FinalArgumentsBufferedBeforeOutputItemAdded covers
// function_call_arguments.done arriving before output_item.added. The done
// payload is the authoritative final snapshot: it must replace any partially
// buffered deltas, be flushed exactly once when the item is announced, and
// late deltas must not corrupt it.
func TestResponseStream_FinalArgumentsBufferedBeforeOutputItemAdded(t *testing.T) {
	t.Parallel()

	const args = `{"cmd":"ls"}`

	argumentsDone := map[string]any{
		"type":      "response.function_call_arguments.done",
		"item_id":   "fc_1",
		"name":      "shell",
		"arguments": args,
	}
	outputItemAdded := map[string]any{
		"type":    "response.output_item.added",
		"item_id": "fc_1",
		"item": map[string]any{
			"type":    "function_call",
			"id":      "fc_1",
			"call_id": "call_1",
			"name":    "shell",
		},
	}
	outputItemDone := map[string]any{
		"type":    "response.output_item.done",
		"item_id": "fc_1",
		"item": map[string]any{
			"type":      "function_call",
			"id":        "fc_1",
			"call_id":   "call_1",
			"name":      "shell",
			"arguments": args,
			"status":    "completed",
		},
	}

	tests := []struct {
		name   string
		events []map[string]any
	}{
		{
			name: "arguments done then output item added",
			events: []map[string]any{
				argumentsDone,
				outputItemAdded,
				outputItemDone,
				toolCallsCompletedEvent("resp_early_args_done"),
			},
		},
		{
			name: "late delta after arguments done is ignored",
			events: []map[string]any{
				argumentsDone,
				{
					"type":    "response.function_call_arguments.delta",
					"item_id": "fc_1",
					"delta":   "EXTRA",
				},
				outputItemAdded,
				outputItemDone,
				toolCallsCompletedEvent("resp_early_args_done_late_delta"),
			},
		},
		{
			name: "arguments done replaces partially buffered deltas",
			events: []map[string]any{
				{
					"type":    "response.function_call_arguments.delta",
					"item_id": "fc_1",
					"delta":   `{"cmd":`,
				},
				argumentsDone,
				outputItemAdded,
				outputItemDone,
				toolCallsCompletedEvent("resp_early_args_done_partial_deltas"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			adapter := newResponseStreamAdapter(&fakeEventStream{events: decodeEvents(t, tt.events)}, true)

			calls, finishReason := drainToolCalls(t, adapter)
			require.Len(t, calls, 1)
			assert.Equal(t, "shell", calls["call_1"].name)
			assert.JSONEq(t, args, calls["call_1"].args)
			assert.Equal(t, chat.FinishReasonToolCalls, finishReason)
		})
	}
}

// TestResponseStream_CopilotUnstableItemIDsOutputTextNotDuplicated is a
// regression test for https://github.com/docker/docker-agent/issues/3839.
//
// GitHub Copilot (api_type=openai_responses) assigns inconsistent item IDs
// within a single output: output_item.added, content_part.added, each
// output_text.delta and output_item.done can all carry distinct IDs, while
// output_index stays stable. Deduplication keyed on item IDs alone then
// misses the streamed deltas and re-emits the output_item.done snapshot,
// doubling structured output. The snapshot must still be emitted when no
// deltas arrived at all: content_part.added stays structural and must not
// count as emitted content. Tracking is per output_index slot: content
// streamed for one output must not suppress the snapshot fallback of a
// sibling output at another index.
func TestResponseStream_CopilotUnstableItemIDsOutputTextNotDuplicated(t *testing.T) {
	t.Parallel()

	const text = `{"answer":"structured","confidence":0.9}`
	const secondText = "A second, distinct message."

	outputItemAdded := map[string]any{
		"type":         "response.output_item.added",
		"output_index": 0,
		"item": map[string]any{
			"type": "message",
			"id":   "msg_A",
			"role": "assistant",
		},
	}
	contentPartAdded := map[string]any{
		"type":          "response.content_part.added",
		"item_id":       "msg_B",
		"output_index":  0,
		"content_index": 0,
		"part": map[string]any{
			"type": "output_text",
			"text": text,
		},
	}
	outputItemDone := map[string]any{
		"type":         "response.output_item.done",
		"output_index": 0,
		"item": map[string]any{
			"type": "message",
			"id":   "msg_A",
			"role": "assistant",
			"content": []any{
				map[string]any{"type": "output_text", "text": text},
			},
			"status": "completed",
		},
	}
	completed := map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id":     "resp_copilot_ids",
			"output": []any{},
			"usage": map[string]any{
				"input_tokens":          4,
				"output_tokens":         4,
				"total_tokens":          8,
				"input_tokens_details":  map[string]any{"cached_tokens": 0},
				"output_tokens_details": map[string]any{"reasoning_tokens": 0},
			},
		},
	}

	tests := []struct {
		name        string
		events      []map[string]any
		wantContent string
	}{
		{
			name: "deltas under a different item id are not re-emitted by the done snapshot",
			events: []map[string]any{
				outputItemAdded,
				contentPartAdded,
				{
					"type":         "response.output_text.delta",
					"item_id":      "msg_C",
					"output_index": 0,
					"delta":        `{"answer":"structured",`,
				},
				{
					"type":         "response.output_text.delta",
					"item_id":      "msg_C",
					"output_index": 0,
					"delta":        `"confidence":0.9}`,
				},
				outputItemDone,
				completed,
			},
			wantContent: text,
		},
		{
			name: "without deltas the done snapshot is still emitted once",
			events: []map[string]any{
				outputItemAdded,
				contentPartAdded,
				outputItemDone,
				completed,
			},
			wantContent: text,
		},
		{
			name: "streamed content at index 0 does not suppress the snapshot of a second output",
			events: []map[string]any{
				outputItemAdded,
				contentPartAdded,
				{
					"type":         "response.output_text.delta",
					"item_id":      "msg_C",
					"output_index": 0,
					"delta":        text,
				},
				outputItemDone,
				{
					"type":         "response.output_item.done",
					"output_index": 1,
					"item": map[string]any{
						"type": "message",
						"id":   "msg_D",
						"role": "assistant",
						"content": []any{
							map[string]any{"type": "output_text", "text": secondText},
						},
						"status": "completed",
					},
				},
				completed,
			},
			wantContent: text + secondText,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			adapter := newResponseStreamAdapter(&fakeEventStream{events: decodeEvents(t, tt.events)}, true)

			content, finishReason := drainContent(t, adapter)
			assert.Equal(t, tt.wantContent, content)
			assert.Equal(t, chat.FinishReasonStop, finishReason)
		})
	}
}
