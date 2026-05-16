package anthropic

import (
	"fmt"
	"log/slog"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/tools"
)

// betaStreamAdapter adapts the Anthropic Beta stream to our interface
type betaStreamAdapter struct {
	retryableStream[anthropic.BetaRawMessageStreamEventUnion]

	trackUsage bool
	toolCall   bool
	toolID     string
}

// newBetaStreamAdapter creates a new Beta stream adapter
func (c *Client) newBetaStreamAdapter(stream *ssestream.Stream[anthropic.BetaRawMessageStreamEventUnion], trackUsage bool) *betaStreamAdapter {
	return &betaStreamAdapter{
		retryableStream: retryableStream[anthropic.BetaRawMessageStreamEventUnion]{stream: stream},
		trackUsage:      trackUsage,
	}
}

// Recv gets the next completion chunk from the Beta stream
func (a *betaStreamAdapter) Recv() (chat.MessageStreamResponse, error) {
	ok, err := a.next()
	if !ok {
		return chat.MessageStreamResponse{}, wrapAnthropicError(err)
	}

	event := a.stream.Current()

	response := chat.MessageStreamResponse{
		ID:     event.Message.ID,
		Object: "chat.completion.chunk",
		Model:  event.Message.Model,
		Choices: []chat.MessageStreamChoice{
			{
				Index: 0,
				Delta: chat.MessageDelta{
					Role: string(chat.MessageRoleAssistant),
				},
			},
		},
	}

	// Handle different event types
	switch eventVariant := event.AsAny().(type) {
	case anthropic.BetaRawContentBlockStartEvent:
		switch block := eventVariant.ContentBlock.AsAny().(type) {
		case anthropic.BetaToolUseBlock:
			a.toolID = block.ID
			a.toolCall = true
			toolCall := tools.ToolCall{
				ID:   a.toolID,
				Type: "function",
				Function: tools.FunctionCall{
					Name: block.Name,
				},
			}
			response.Choices[0].Delta.ToolCalls = []tools.ToolCall{toolCall}
		case anthropic.BetaThinkingBlock:
			if block.Thinking != "" {
				response.Choices[0].Delta.ReasoningContent = block.Thinking
				slog.Debug("Received thinking", "thinking", block.Thinking)
			}
			if block.Signature != "" {
				response.Choices[0].Delta.ThinkingSignature = block.Signature
			}
		}
	case anthropic.BetaRawContentBlockDeltaEvent:
		switch deltaVariant := eventVariant.Delta.AsAny().(type) {
		case anthropic.BetaTextDelta:
			response.Choices[0].Delta.Content = deltaVariant.Text
		case anthropic.BetaThinkingDelta:
			response.Choices[0].Delta.ReasoningContent = deltaVariant.Thinking
		case anthropic.BetaInputJSONDelta:
			inputBytes := deltaVariant.PartialJSON
			toolCall := tools.ToolCall{
				ID:   a.toolID,
				Type: "function",
				Function: tools.FunctionCall{
					Arguments: inputBytes,
				},
			}
			response.Choices[0].Delta.ToolCalls = []tools.ToolCall{toolCall}
		case anthropic.BetaSignatureDelta:
			// Signature delta is for thinking blocks - capture it so we can replay thinking in history
			response.Choices[0].Delta.ThinkingSignature = deltaVariant.Signature
		default:
			return response, fmt.Errorf("unknown delta type: %T", deltaVariant)
		}
	case anthropic.BetaRawMessageDeltaEvent:
		if a.trackUsage {
			response.Usage = &chat.Usage{
				InputTokens:       eventVariant.Usage.InputTokens,
				OutputTokens:      eventVariant.Usage.OutputTokens,
				CachedInputTokens: eventVariant.Usage.CacheReadInputTokens,
				CacheWriteTokens:  eventVariant.Usage.CacheCreationInputTokens,
			}
		}
	case anthropic.BetaRawMessageStopEvent:
		if a.toolCall {
			response.Choices[0].FinishReason = chat.FinishReasonToolCalls
		} else {
			response.Choices[0].FinishReason = chat.FinishReasonStop
		}
	}

	return response, nil
}

// Close closes the Beta stream
func (a *betaStreamAdapter) Close() {
	a.stream.Close()
}
