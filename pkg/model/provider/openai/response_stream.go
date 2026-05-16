package openai

import (
	"cmp"
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
}

func newResponseStreamAdapter(stream responseEventStream, trackUsage bool) *ResponseStreamAdapter {
	return &ResponseStreamAdapter{
		stream:         stream,
		trackUsage:     trackUsage,
		itemCallIDMap:  make(map[string]string),
		itemHasContent: make(map[string]bool),
	}
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
			a.itemHasContent[event.ItemID] = true
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
	case "response.content_part.added":
		if event.Part.Type == "text" {
			content := event.Part.Text
			if content != "" {
				a.itemHasContent[event.ItemID] = true
				response.Choices = []chat.MessageStreamChoice{
					{
						Delta: chat.MessageDelta{
							Content: content,
							Role:    "assistant",
						},
					},
				}
			}
		} else {
			slog.Warn("Received function_call_arguments.delta for unknown item_id", "item_id", event.ItemID, "known_items", len(a.itemCallIDMap))
		}
	case "response.content_part.delta":
		content := cmp.Or(event.Delta, event.Text, event.Code, event.Part.Text)
		if content != "" {
			a.itemHasContent[event.ItemID] = true
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

			// Only emit the tool call with name, arguments will come in delta events
			if funcName != "" {
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
										Arguments: "", // Arguments come in delta events
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
		if callID, ok := a.itemCallIDMap[event.ItemID]; ok {
			args := cmp.Or(event.Delta, event.Arguments)

			slog.Debug("Emitting arguments delta", "item_id", event.ItemID, "call_id", callID, "delta_length", len(args), "delta_preview", args[:min(len(args), 20)])

			if args != "" {
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
			slog.Error("Unknown item_id for arguments delta", "item_id", event.ItemID, "known_items", a.itemCallIDMap)
		}
	case "response.function_call_arguments.done":
		// Function call arguments are complete - we already streamed them
		slog.Debug("Function call arguments done", "item_id", event.ItemID, "call_id", a.itemCallIDMap[event.ItemID])

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
		slog.Debug("Output item done", "item_id", event.ItemID, "type", event.Item.Type)
		// Don't set finish reason here - wait for response.completed
		// Just handle any missed content
		if event.Item.Type == "message" && !a.itemHasContent[event.ItemID] {
			for _, content := range event.Item.Content {
				if content.Type == "text" && content.Text != "" {
					response.Choices = append(response.Choices, chat.MessageStreamChoice{
						Delta: chat.MessageDelta{
							Content: content.Text,
							Role:    "assistant",
						},
					})
					a.itemHasContent[event.ItemID] = true
				}
			}
		}

	case "response.done", "response.completed":
		slog.Info("Response done received", "event_type", event.Type)
		// Extract usage
		u := event.Response.Usage
		if u.TotalTokens > 0 {
			response.Usage = &chat.Usage{
				InputTokens:       u.InputTokens - u.InputTokensDetails.CachedTokens,
				OutputTokens:      u.OutputTokens,
				CachedInputTokens: u.InputTokensDetails.CachedTokens,
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
	default:
		slog.Info("Unhandled stream event type", "type", event.Type)
	}

	return response, nil
}

// Close closes the stream
func (a *ResponseStreamAdapter) Close() {
	_ = a.stream.Close()
}
