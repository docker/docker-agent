package openai

import (
	"cmp"
	"fmt"
	"io"
	"log/slog"

	"github.com/openai/openai-go/v3/packages/ssestream"
	"github.com/openai/openai-go/v3/responses"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider/oaistream"
	"github.com/docker/docker-agent/pkg/tools"
)

// Compile-time check: ssestream.Stream satisfies responseEventStream.
var _ responseEventStream = (*ssestream.Stream[responses.ResponseStreamEventUnion])(nil)

// ResponseStreamAdapter adapts the OpenAI responses stream to our interface.
// It works with any responseEventStream implementation (SSE or WebSocket).
type ResponseStreamAdapter struct {
	stream         responseEventStream
	trackUsage     bool
	itemCallIDMap  map[string]string
	itemHasContent map[string]bool
	// outputIndexHasContent mirrors itemHasContent keyed by output_index.
	// The key identifies an output slot of the response, not a specific item:
	// all events sharing an output_index belong to the same output whatever
	// item IDs they carry, and distinct slots are deduplicated independently.
	// Some providers (GitHub Copilot) use inconsistent item IDs across the
	// events of a single output while output_index stays stable, so an
	// ID-only lookup misses the streamed deltas and would re-emit the
	// output_item.done snapshot, doubling the text. Only events that actually
	// carry an output_index participate (checked via JSON metadata, since the
	// scalar field cannot distinguish absent from a legitimate 0), so streams
	// omitting it cannot collide on the zero value.
	outputIndexHasContent map[int64]bool
	itemHasArgs           map[string]bool
	pendingArgs           map[string]string
	// itemArgsFinal marks items whose complete final arguments were already
	// received: emitted (function_call_arguments.done, output_item.done
	// snapshot), buffered in pendingArgs, or streamed as deltas and confirmed
	// by a non-empty function_call_arguments.done. Distinct from itemHasArgs,
	// which only means some argument bytes were emitted: any later arguments
	// delta for such an item is stale and must be dropped.
	itemArgsFinal map[string]bool
}

func newResponseStreamAdapter(stream responseEventStream, trackUsage bool) *ResponseStreamAdapter {
	return &ResponseStreamAdapter{
		stream:                stream,
		trackUsage:            trackUsage,
		itemCallIDMap:         make(map[string]string),
		itemHasContent:        make(map[string]bool),
		outputIndexHasContent: make(map[int64]bool),
		itemHasArgs:           make(map[string]bool),
		pendingArgs:           make(map[string]string),
		itemArgsFinal:         make(map[string]bool),
	}
}

func isTextContentPart(partType string) bool {
	return partType == "text" || partType == "output_text"
}

// markContentEmitted records that text was emitted for this output, keyed by
// item ID and, when the event carries one, by output_index.
func (a *ResponseStreamAdapter) markContentEmitted(event responses.ResponseStreamEventUnion, itemID string) {
	a.itemHasContent[itemID] = true
	if event.JSON.OutputIndex.Valid() {
		a.outputIndexHasContent[event.OutputIndex] = true
	}
}

// hasEmittedContent reports whether text for this output was already emitted,
// matching by item ID or by output_index when the event carries one.
func (a *ResponseStreamAdapter) hasEmittedContent(event responses.ResponseStreamEventUnion, itemID string) bool {
	return a.itemHasContent[itemID] ||
		(event.JSON.OutputIndex.Valid() && a.outputIndexHasContent[event.OutputIndex])
}

// Recv gets the next completion chunk
func (a *ResponseStreamAdapter) Recv() (chat.MessageStreamResponse, error) {
	if !a.stream.Next() {
		if err := a.stream.Err(); err != nil {
			return chat.MessageStreamResponse{}, oaistream.WrapOpenAIError(err)
		}
		return chat.MessageStreamResponse{}, io.EOF
	}

	event := a.stream.Current()
	slog.Debug("Stream event received", "type", event.Type)
	response := chat.MessageStreamResponse{}

	switch event.Type {
	case "response.output_text.delta":
		content := cmp.Or(event.Delta, event.Text)
		if content != "" {
			a.markContentEmitted(event, event.ItemID)
			response.Choices = []chat.MessageStreamChoice{
				{
					Delta: chat.MessageDelta{
						Content: content,
						Role:    "assistant",
					},
				},
			}
		}
	case "response.output_text.done":
		slog.Debug("Output text done", "item_id", event.ItemID)
	case "response.created", "response.in_progress", "response.content_part.done":
		slog.Debug("Ignoring structural response stream event", "type", event.Type, "item_id", event.ItemID)
	case "response.content_part.added":
		// This event announces that a content part exists. For output_text
		// parts, the text itself is streamed separately via
		// response.output_text.delta and finalized by content_part.done /
		// output_item.done. Do not emit Part.Text here: newer Responses API
		// models can include a snapshot in the added event, and emitting it
		// duplicates the subsequent deltas.
		if !isTextContentPart(event.Part.Type) {
			slog.Debug("Ignoring non-text response content part", "item_id", event.ItemID, "part_type", event.Part.Type)
		}
	case "response.content_part.delta":
		content := cmp.Or(event.Delta, event.Text, event.Code, event.Part.Text)
		if content != "" {
			a.markContentEmitted(event, event.ItemID)
			response.Choices = []chat.MessageStreamChoice{
				{
					Delta: chat.MessageDelta{
						Content: content,
						Role:    "assistant",
					},
				},
			}
		}
	case "response.output_item.added":
		// Check for function call
		// The item.type is "function_call" for tool calls in the Response API
		if event.Item.Type == "function_call" {
			callID := cmp.Or(event.Item.CallID, event.Item.ID, event.ItemID)
			// Use Item.ID as the map key, since arguments deltas use the item_id field
			// which corresponds to the Item.ID from the output_item.added event
			itemID := event.Item.ID
			if itemID == "" {
				itemID = event.ItemID // Fallback if Item.ID is somehow empty
			}
			a.itemCallIDMap[itemID] = callID

			// Try to get the function name from top-level Name field, then Item.Name
			funcName := cmp.Or(event.Name, event.Item.Name)
			if funcName != "" && event.Name == "" {
				slog.Debug("Extracted name from Item.Name field", "name", funcName)
			}

			// Only emit the tool call with name. Arguments normally arrive in
			// delta events, but some transports/models can deliver an arguments
			// delta before the output_item.added event. Flush any such buffered
			// bytes with the first named tool-call delta so the runtime can still
			// reconstruct the call.
			if funcName != "" {
				// itemArgsFinal is intentionally kept: if the flushed buffer
				// held the final arguments, later deltas must stay ignored.
				args := a.pendingArgs[itemID]
				delete(a.pendingArgs, itemID)
				if args != "" {
					a.itemHasArgs[itemID] = true
				}

				slog.Debug("Emitting tool call with name", "item_id", event.ItemID, "call_id", callID, "name", funcName)
				response.Choices = []chat.MessageStreamChoice{
					{
						Delta: chat.MessageDelta{
							ToolCalls: []tools.ToolCall{
								{
									ID:   callID,
									Type: "function",
									Function: tools.FunctionCall{
										Name:      funcName,
										Arguments: args,
									},
								},
							},
						},
					},
				}
			}
		}
	case "response.function_call_arguments.delta":
		// Handle function call arguments delta
		slog.Debug("Function call arguments delta received", "item_id", event.ItemID)
		if a.itemArgsFinal[event.ItemID] {
			// The complete arguments were already emitted or buffered;
			// appending a late delta would corrupt that JSON.
			slog.Debug("Ignoring arguments delta after final arguments", "item_id", event.ItemID)
		} else if callID, ok := a.itemCallIDMap[event.ItemID]; ok {
			args := cmp.Or(event.Delta, event.Arguments)

			slog.Debug("Emitting arguments delta", "item_id", event.ItemID, "call_id", callID, "delta_length", len(args), "delta_preview", args[:min(len(args), 20)])

			if args != "" {
				a.itemHasArgs[event.ItemID] = true
				response.Choices = []chat.MessageStreamChoice{
					{
						Delta: chat.MessageDelta{
							ToolCalls: []tools.ToolCall{
								{
									ID:   callID,
									Type: "function",
									Function: tools.FunctionCall{
										Arguments: args,
									},
								},
							},
						},
					},
				}
			}
		} else {
			args := cmp.Or(event.Delta, event.Arguments)
			if args != "" {
				a.pendingArgs[event.ItemID] += args
				slog.Debug("Buffered function call arguments delta before output item", "item_id", event.ItemID, "delta_length", len(args))
			}
		}
	case "response.function_call_arguments.done":
		// Arguments normally arrive via delta events, making this event
		// redundant. Some Responses API implementations skip the deltas and
		// only deliver the complete arguments here, so emit them once in that
		// case.
		slog.Debug("Function call arguments done", "item_id", event.ItemID, "call_id", a.itemCallIDMap[event.ItemID])
		if args := event.Arguments; args != "" {
			// A non-empty payload is by definition the complete final
			// arguments, so any later delta for this item is stale and must
			// be dropped, even when deltas already streamed the arguments.
			a.itemArgsFinal[event.ItemID] = true
			if !a.itemHasArgs[event.ItemID] {
				if callID, ok := a.itemCallIDMap[event.ItemID]; ok {
					slog.Debug("Emitting final arguments from arguments done event", "item_id", event.ItemID, "call_id", callID, "args_length", len(args))
					a.itemHasArgs[event.ItemID] = true
					response.Choices = []chat.MessageStreamChoice{
						{
							Delta: chat.MessageDelta{
								ToolCalls: []tools.ToolCall{
									{
										ID:   callID,
										Type: "function",
										Function: tools.FunctionCall{
											Arguments: args,
										},
									},
								},
							},
						},
					}
				} else {
					// The function item was not announced yet. This payload is
					// the authoritative final snapshot: replace any partially
					// buffered deltas with it.
					a.pendingArgs[event.ItemID] = args
					slog.Debug("Buffered final arguments before output item", "item_id", event.ItemID, "args_length", len(args))
				}
			}
		}

	case "response.reasoning_text.delta":
		// Handle reasoning text deltas (thinking traces from reasoning models)
		content := event.Delta
		if content != "" {
			slog.Debug("Reasoning text delta received", "item_id", event.ItemID, "delta_length", len(content))
			response.Choices = []chat.MessageStreamChoice{
				{
					Delta: chat.MessageDelta{
						ReasoningContent: content,
						Role:             "assistant",
					},
				},
			}
		}
	case "response.reasoning_text.done":
		slog.Debug("Reasoning text done", "item_id", event.ItemID)

	case "response.reasoning_summary_text.delta":
		// Handle reasoning summary text deltas
		content := event.Delta
		if content != "" {
			slog.Debug("Reasoning summary text delta received", "item_id", event.ItemID, "delta_length", len(content))
			response.Choices = []chat.MessageStreamChoice{
				{
					Delta: chat.MessageDelta{
						ReasoningContent: content,
						Role:             "assistant",
					},
				},
			}
		}
	case "response.reasoning_summary_text.done":
		slog.Debug("Reasoning summary text done", "item_id", event.ItemID)
	case "response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		slog.Debug("Reasoning summary part event", "type", event.Type, "item_id", event.ItemID)

	case "response.output_item.done":
		// Tool call or message item is complete
		itemID := cmp.Or(event.ItemID, event.Item.ID)
		slog.Debug("Output item done", "item_id", itemID, "type", event.Item.Type)
		// Don't set finish reason here - wait for response.completed.
		// Just handle any missed content. Some Responses API transports omit
		// the top-level item_id on output_item.done while still providing
		// item.id, so use the resolved itemID for deduplication. Others
		// (GitHub Copilot) use different item IDs for the deltas and the done
		// event of the same output, so also match on output_index.
		if event.Item.Type == "message" && !a.hasEmittedContent(event, itemID) {
			for _, content := range event.Item.Content {
				if isTextContentPart(content.Type) && content.Text != "" {
					response.Choices = append(response.Choices, chat.MessageStreamChoice{
						Delta: chat.MessageDelta{
							Content: content.Text,
							Role:    "assistant",
						},
					})
					a.markContentEmitted(event, itemID)
				}
			}
		}
		// Last-resort fallback for function calls whose arguments were neither
		// streamed via deltas nor delivered by function_call_arguments.done:
		// recover them from the completed item snapshot.
		if event.Item.Type == "function_call" && !a.itemHasArgs[itemID] {
			if args := event.Item.Arguments.OfString; args != "" {
				callID := cmp.Or(a.itemCallIDMap[itemID], event.Item.CallID, itemID)
				slog.Debug("Emitting final arguments from output item snapshot", "item_id", itemID, "call_id", callID, "args_length", len(args))
				a.itemHasArgs[itemID] = true
				a.itemArgsFinal[itemID] = true
				response.Choices = append(response.Choices, chat.MessageStreamChoice{
					Delta: chat.MessageDelta{
						ToolCalls: []tools.ToolCall{
							{
								ID:   callID,
								Type: "function",
								Function: tools.FunctionCall{
									Arguments: args,
								},
							},
						},
					},
				})
			}
		}

	case "response.done", "response.completed":
		slog.Info("Response done received", "event_type", event.Type)
		// Extract usage
		u := event.Response.Usage
		if u.TotalTokens > 0 {
			// chat.Usage treats InputTokens, CachedInputTokens and
			// CacheWriteTokens as mutually exclusive buckets, while the
			// provider's input_tokens_details is a breakdown of input_tokens.
			// Subtract both detail counts so InputTokens is only the fresh
			// remainder and the three buckets sum back to input_tokens.
			response.Usage = &chat.Usage{
				InputTokens:       u.InputTokens - u.InputTokensDetails.CachedTokens - u.InputTokensDetails.CacheWriteTokens,
				OutputTokens:      u.OutputTokens,
				CachedInputTokens: u.InputTokensDetails.CachedTokens,
				CacheWriteTokens:  u.InputTokensDetails.CacheWriteTokens,
				ReasoningTokens:   u.OutputTokensDetails.ReasoningTokens,
			}
		}
		// Check if there were any tool calls in the output
		hasToolCalls := false
		for _, output := range event.Response.Output {
			if output.Type == "function_call" {
				hasToolCalls = true
				break
			}
		}
		finishReason := chat.FinishReasonStop
		if hasToolCalls {
			finishReason = chat.FinishReasonToolCalls
		}
		response.Choices = []chat.MessageStreamChoice{
			{
				FinishReason: finishReason,
			},
		}
	case "response.incomplete":
		// Terminal event: the model stopped before producing a complete
		// response. incomplete_details.reason is "max_output_tokens" or
		// "content_filter". Usage is still reported and must not be dropped,
		// and the reason must reach the loop as a finish reason so the empty
		// turn is explained rather than reported as "stop reason: null".
		reason := event.Response.IncompleteDetails.Reason
		slog.Debug("Response incomplete",
			"reason", reason,
			"response_id", event.Response.ID,
			"output_items", len(event.Response.Output),
			"output_tokens", event.Response.Usage.OutputTokens,
			"reasoning_tokens", event.Response.Usage.OutputTokensDetails.ReasoningTokens,
			"response_raw", event.Response.RawJSON(),
		)
		u := event.Response.Usage
		if u.TotalTokens > 0 {
			response.Usage = &chat.Usage{
				InputTokens:       u.InputTokens - u.InputTokensDetails.CachedTokens - u.InputTokensDetails.CacheWriteTokens,
				OutputTokens:      u.OutputTokens,
				CachedInputTokens: u.InputTokensDetails.CachedTokens,
				CacheWriteTokens:  u.InputTokensDetails.CacheWriteTokens,
				ReasoningTokens:   u.OutputTokensDetails.ReasoningTokens,
			}
		}
		finishReason := chat.FinishReasonLength
		if reason == "content_filter" {
			finishReason = chat.FinishReasonRefusal
		}
		response.Choices = []chat.MessageStreamChoice{{FinishReason: finishReason}}

	case "response.failed":
		// Terminal event: the provider failed the response after accepting
		// the request. Surface it as an error so the loop does not report an
		// empty turn.
		e := event.Response.Error
		slog.Error("Response failed", "code", e.Code, "message", e.Message, "response_id", event.Response.ID)
		return chat.MessageStreamResponse{}, fmt.Errorf("openai response failed (%s): %s [response_id=%s]", e.Code, e.Message, event.Response.ID)

	default:
		slog.Info("Unhandled stream event type", "type", event.Type)
		slog.Debug("Unhandled stream event payload", "type", event.Type, "raw", event.RawJSON())
	}

	return response, nil
}

// Close closes the stream
func (a *ResponseStreamAdapter) Close() {
	_ = a.stream.Close()
}
