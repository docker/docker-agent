package anthropic

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/tools"
)

// streamAdapter adapts the Anthropic stream to our interface
type streamAdapter struct {
	retryableStream

	trackUsage bool
	toolCall   bool
	stopReason StopReason
	// toolIDByBlock maps a content block index to its tool_use block ID.
	toolIDByBlock map[int64]string
	message       Message
	rawLost       bool
}

func (c *Client) newStreamAdapter(stream *EventStream, trackUsage bool) *streamAdapter {
	return &streamAdapter{
		retryableStream: retryableStream{stream: stream},
		trackUsage:      trackUsage,
		toolIDByBlock:   map[int64]string{},
	}
}

// isContextLengthError checks if the error indicates context window exceeded.
func isContextLengthError(err error) bool {
	if err == nil {
		return false
	}

	apiErr, ok := errors.AsType[*APIError](err)
	if !ok || apiErr.StatusCode != http.StatusBadRequest {
		return false
	}

	var errResp struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(apiErr.RawJSON()), &errResp) != nil {
		return false
	}

	if errResp.Error.Type != "invalid_request_error" {
		return false
	}

	msg := errResp.Error.Message
	return strings.Contains(msg, "prompt is too long") ||
		strings.Contains(msg, "too many tokens") ||
		strings.Contains(msg, "context length") ||
		strings.Contains(msg, "maximum context")
}

// Recv gets the next completion chunk
func (a *streamAdapter) Recv() (chat.MessageStreamResponse, error) {
	ok, err := a.next()
	if !ok {
		return chat.MessageStreamResponse{}, wrapAnthropicError(err)
	}

	event := a.stream.Current()
	a.accumulate(event)

	var msgID, msgModel string
	if event.Message != nil {
		msgID = event.Message.ID
		msgModel = event.Message.Model
	}
	response := chat.MessageStreamResponse{
		ID:     msgID,
		Object: "chat.completion.chunk",
		Model:  msgModel,
		Choices: []chat.MessageStreamChoice{
			{
				Index: 0,
				Delta: chat.MessageDelta{
					Role: string(chat.MessageRoleAssistant),
				},
			},
		},
	}

	switch event.Type {
	case "content_block_start":
		if block := event.ContentBlock; block != nil {
			switch block.Type {
			case "tool_use":
				a.toolIDByBlock[event.Index] = block.ID
				a.toolCall = true
				toolCall := tools.ToolCall{
					ID:   block.ID,
					Type: "function",
					Function: tools.FunctionCall{
						Name: block.Name,
					},
				}
				response.Choices[0].Delta.ToolCalls = []tools.ToolCall{toolCall}
			case "thinking":
				if block.Thinking != "" {
					response.Choices[0].Delta.ReasoningContent = block.Thinking
				}
				if block.Signature != "" {
					response.Choices[0].Delta.ThinkingSignature = block.Signature
				}
			}
		}
	case "content_block_delta":
		if delta := event.Delta; delta != nil {
			switch delta.Type {
			case "text_delta":
				response.Choices[0].Delta.Content = delta.Text
			case "thinking_delta":
				response.Choices[0].Delta.ReasoningContent = delta.Thinking
			case "signature_delta":
				response.Choices[0].Delta.ThinkingSignature = delta.Signature
			case "input_json_delta":
				toolCall := tools.ToolCall{
					ID:   a.toolIDByBlock[event.Index],
					Type: "function",
					Function: tools.FunctionCall{
						Arguments: delta.PartialJSON,
					},
				}
				response.Choices[0].Delta.ToolCalls = []tools.ToolCall{toolCall}
			default:
				return response, fmt.Errorf("unknown delta type: %s", delta.Type)
			}
		}
	case "message_delta":
		if event.Delta != nil {
			a.stopReason = event.Delta.StopReason
		}
		if a.trackUsage {
			response.Usage = usageFromMessage(a.message.Usage)
		}
	case "message_stop":
		response.Choices[0].FinishReason = finishReason(a.stopReason, a.toolCall)
		if !a.rawLost {
			response.Choices[0].Delta.ProviderState = newProviderState(a.message.ID, a.message.Model, a.message.Content)
		}
	}

	return response, nil
}

func (a *streamAdapter) accumulate(event StreamEvent) {
	if a.rawLost && event.Type != "message_start" && event.Type != "message_delta" {
		return
	}
	if err := a.message.Accumulate(event); err != nil {
		a.rawLost = true
		slog.Debug("Anthropic: raw response capture disabled", "error", err)
	}
}

// usageFromMessage maps accumulated usage; reasoning is already included in output.
func usageFromMessage(u Usage) *chat.Usage {
	return &chat.Usage{
		InputTokens:       u.InputTokens,
		OutputTokens:      u.OutputTokens,
		CachedInputTokens: u.CacheReadInputTokens,
		CacheWriteTokens:  u.CacheCreationInputTokens,
		ReasoningTokens:   u.OutputTokensDetails.ThinkingTokens,
	}
}

func finishReason(stopReason StopReason, sawToolUse bool) chat.FinishReason {
	switch stopReason {
	case StopReasonToolUse:
		return chat.FinishReasonToolCalls
	case StopReasonMaxTokens, StopReasonModelContextWindowExceeded:
		return chat.FinishReasonLength
	case StopReasonRefusal:
		return chat.FinishReasonRefusal
	}
	if sawToolUse {
		return chat.FinishReasonToolCalls
	}
	return chat.FinishReasonStop
}

func (a *streamAdapter) Close() {
	a.close()
}
