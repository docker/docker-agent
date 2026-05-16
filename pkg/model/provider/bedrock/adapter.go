package bedrock

import (
	"fmt"
	"io"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/tools"
)

// streamAdapter adapts Bedrock's ConverseStreamEventStream to chat.MessageStream
type streamAdapter struct {
	stream     *bedrockruntime.ConverseStreamEventStream
	model      string
	trackUsage bool

	// State for accumulating tool call data
	currentToolID   string
	currentToolName string

	// Buffered state for proper event ordering
	// Bedrock sends MessageStop before Metadata, but runtime expects usage before FinishReason
	pendingFinishReason chat.FinishReason
	pendingUsage        *chat.Usage
	metadataReceived    bool
}

func newStreamAdapter(stream *bedrockruntime.ConverseStreamEventStream, model string, trackUsage bool) *streamAdapter {
	return &streamAdapter{
		stream:     stream,
		model:      model,
		trackUsage: trackUsage,
	}
}

// Recv gets the next completion chunk
func (a *streamAdapter) Recv() (chat.MessageStreamResponse, error) {
	// If we have both finish reason and usage buffered, emit the final response
	// This handles both event orderings: MessageStop→Metadata and Metadata→MessageStop
	if a.pendingFinishReason != "" && a.metadataReceived {
		slog.Debug("Bedrock stream: emitting buffered final response",
			"finish_reason", a.pendingFinishReason,
			"has_usage", a.pendingUsage != nil)
		response := chat.MessageStreamResponse{
			Object: "chat.completion.chunk",
			Model:  a.model,
			Choices: []chat.MessageStreamChoice{
				{
					Index:        0,
					FinishReason: a.pendingFinishReason,
					Delta: chat.MessageDelta{
						Role: string(chat.MessageRoleAssistant),
					},
				},
			},
			Usage: a.pendingUsage,
		}
		// Clear pending state
		a.pendingFinishReason = ""
		a.pendingUsage = nil
		a.metadataReceived = false
		return response, nil
	}

	event, ok := <-a.stream.Events()
	if !ok {
		// Check for errors
		if err := a.stream.Err(); err != nil {
			slog.Debug("Bedrock stream: error on channel close", "error", err)
			return chat.MessageStreamResponse{}, wrapBedrockError(err)
		}
		// If we have a pending finish reason but never got metadata, emit it now
		if a.pendingFinishReason != "" {
			slog.Debug("Bedrock stream: channel closed, emitting pending finish reason without metadata",
				"finish_reason", a.pendingFinishReason,
				"has_usage", a.pendingUsage != nil)
			response := chat.MessageStreamResponse{
				Object: "chat.completion.chunk",
				Model:  a.model,
				Choices: []chat.MessageStreamChoice{
					{
						Index:        0,
						FinishReason: a.pendingFinishReason,
						Delta: chat.MessageDelta{
							Role: string(chat.MessageRoleAssistant),
						},
					},
				},
				Usage: a.pendingUsage,
			}
			a.pendingFinishReason = ""
			a.pendingUsage = nil
			return response, nil
		}
		slog.Debug("Bedrock stream: channel closed, returning EOF")
		return chat.MessageStreamResponse{}, io.EOF
	}

	response := chat.MessageStreamResponse{
		Object: "chat.completion.chunk",
		Model:  a.model,
		Choices: []chat.MessageStreamChoice{
			{
				Index: 0,
				Delta: chat.MessageDelta{
					Role: string(chat.MessageRoleAssistant),
				},
			},
		},
	}

	switch ev := event.(type) {
	case *types.ConverseStreamOutputMemberMessageStart:
		slog.Debug("Bedrock stream: message start", "role", ev.Value.Role)

	case *types.ConverseStreamOutputMemberContentBlockStart:
		// Handle content block start - tool use or text
		if start, ok := ev.Value.Start.(*types.ContentBlockStartMemberToolUse); ok {
			a.currentToolID = derefString(start.Value.ToolUseId)
			a.currentToolName = derefString(start.Value.Name)

			// Emit initial tool call
			response.Choices[0].Delta.ToolCalls = []tools.ToolCall{{
				ID:   a.currentToolID,
				Type: "function",
				Function: tools.FunctionCall{
					Name: a.currentToolName,
				},
			}}
		}

	case *types.ConverseStreamOutputMemberContentBlockDelta:
		// Handle content block delta - text or tool input
		if ev.Value.Delta != nil {
			switch delta := ev.Value.Delta.(type) {
			case *types.ContentBlockDeltaMemberText:
				response.Choices[0].Delta.Content = delta.Value

			case *types.ContentBlockDeltaMemberToolUse:
				// Emit partial tool call with input delta
				response.Choices[0].Delta.ToolCalls = []tools.ToolCall{{
					ID:   a.currentToolID,
					Type: "function",
					Function: tools.FunctionCall{
						Arguments: derefString(delta.Value.Input),
					},
				}}

			case *types.ContentBlockDeltaMemberReasoningContent:
				// Handle reasoning content (text, signature, redacted)
				switch reasoningDelta := delta.Value.(type) {
				case *types.ReasoningContentBlockDeltaMemberText:
					response.Choices[0].Delta.ReasoningContent = reasoningDelta.Value
				case *types.ReasoningContentBlockDeltaMemberSignature:
					response.Choices[0].Delta.ThinkingSignature = reasoningDelta.Value
				case *types.ReasoningContentBlockDeltaMemberRedactedContent:
					response.Choices[0].Delta.ThinkingSignature = string(reasoningDelta.Value)
				default:
					return chat.MessageStreamResponse{}, fmt.Errorf("unknown reasoning delta type: %T", reasoningDelta)
				}
			}
		}

	case *types.ConverseStreamOutputMemberContentBlockStop:
		slog.Debug("Bedrock stream: content block stop", "index", ev.Value.ContentBlockIndex)

	case *types.ConverseStreamOutputMemberMessageStop:
		// Buffer the finish reason - don't emit it yet, wait for metadata with usage
		// Bedrock sends MessageStop before Metadata, but runtime returns early on FinishReason
		stopReason := ev.Value.StopReason
		switch stopReason {
		case types.StopReasonToolUse:
			a.pendingFinishReason = chat.FinishReasonToolCalls
		case types.StopReasonEndTurn, types.StopReasonStopSequence:
			a.pendingFinishReason = chat.FinishReasonStop
		case types.StopReasonMaxTokens:
			a.pendingFinishReason = chat.FinishReasonLength
		default:
			a.pendingFinishReason = chat.FinishReasonStop
		}
		slog.Debug("Bedrock stream: message stop (buffered)",
			"stop_reason", stopReason,
			"pending_finish_reason", a.pendingFinishReason,
			"metadata_already_received", a.metadataReceived)

	case *types.ConverseStreamOutputMemberMetadata:
		// Metadata event with usage info - capture and mark received
		a.metadataReceived = true
		slog.Debug("Bedrock stream: received metadata event",
			"has_usage", ev.Value.Usage != nil,
			"finish_reason_already_received", a.pendingFinishReason != "")

		if ev.Value.Usage != nil {
			usage := ev.Value.Usage
			slog.Debug("Bedrock stream: usage metadata details",
				"input_tokens", derefInt32(usage.InputTokens),
				"output_tokens", derefInt32(usage.OutputTokens),
				"cache_read_tokens", derefInt32(usage.CacheReadInputTokens),
				"cache_write_tokens", derefInt32(usage.CacheWriteInputTokens),
				"track_usage", a.trackUsage)

			if a.trackUsage {
				a.pendingUsage = &chat.Usage{
					InputTokens:       int64(derefInt32(usage.InputTokens)),
					OutputTokens:      int64(derefInt32(usage.OutputTokens)),
					CachedInputTokens: int64(derefInt32(usage.CacheReadInputTokens)),
					CacheWriteTokens:  int64(derefInt32(usage.CacheWriteInputTokens)),
				}
				slog.Debug("Bedrock stream: usage captured in pendingUsage",
					"input", a.pendingUsage.InputTokens,
					"output", a.pendingUsage.OutputTokens)
			} else {
				slog.Debug("Bedrock stream: usage NOT captured (trackUsage is false)")
			}
		} else {
			slog.Debug("Bedrock stream: metadata event has nil Usage field")
		}

	default:
		slog.Debug("Bedrock stream: unknown event type", "type", fmt.Sprintf("%T", event))
	}

	return response, nil
}

// Close closes the stream
func (a *streamAdapter) Close() {
	if a.stream != nil {
		_ = a.stream.Close()
	}
}

// derefString safely dereferences a string pointer
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// derefInt32 safely dereferences an int32 pointer
func derefInt32(i *int32) int32 {
	if i == nil {
		return 0
	}
	return *i
}
