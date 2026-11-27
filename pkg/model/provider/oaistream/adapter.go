package oaistream

/*
This is a shared adapter for OpenAI-compatible streams.
*/

import (
	"io"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/tools"
)

// StreamAdapter adapts an OpenAI-compatible ChatCompletionChunk stream to chat.MessageStream.
type StreamAdapter struct {
	stream           *ChunkStream
	lastFinishReason chat.FinishReason
	// heldFinishReason is a stop/length held back until the usage chunk
	// arrives; emitted at EOF if no usage chunk ever comes.
	heldFinishReason chat.FinishReason
	toolCalls        map[int]string
	trackUsage       bool
	serviceTier      string
}

func NewStreamAdapter(stream *ChunkStream, trackUsage bool) *StreamAdapter {
	return &StreamAdapter{
		stream:     stream,
		toolCalls:  make(map[int]string),
		trackUsage: trackUsage,
	}
}

// Recv gets the next completion chunk
func (a *StreamAdapter) Recv() (chat.MessageStreamResponse, error) {
	if !a.stream.Next() {
		err := a.stream.Err()
		if err != nil {
			return chat.MessageStreamResponse{}, WrapOpenAIError(err)
		}
		if held := a.heldFinishReason; held != "" {
			a.heldFinishReason = ""
			return chat.MessageStreamResponse{
				Choices: []chat.MessageStreamChoice{{FinishReason: held}},
			}, nil
		}
		return chat.MessageStreamResponse{}, io.EOF
	}

	openaiResponse := a.stream.Current()
	if openaiResponse.ServiceTier != nil && *openaiResponse.ServiceTier != "" {
		a.serviceTier = *openaiResponse.ServiceTier
	}

	response := chat.MessageStreamResponse{
		ID:      openaiResponse.ID,
		Object:  openaiResponse.Object,
		Created: openaiResponse.Created,
		Model:   openaiResponse.Model,
		Choices: make([]chat.MessageStreamChoice, len(openaiResponse.Choices)),
	}

	for i := range openaiResponse.Choices {
		choice := &openaiResponse.Choices[i]

		finishReason := chat.FinishReason(choice.FinishReason)
		if finishReason == "content_filter" {
			finishReason = chat.FinishReasonRefusal
		}
		if a.trackUsage && openaiResponse.Usage == nil &&
			(finishReason == chat.FinishReasonStop || finishReason == chat.FinishReasonLength || finishReason == chat.FinishReasonRefusal) {
			a.heldFinishReason = finishReason
			finishReason = ""
		}

		if finishReason != chat.FinishReasonNull && finishReason != "" {
			a.lastFinishReason = finishReason
		}

		response.Choices[i] = chat.MessageStreamChoice{
			Index:        int(choice.Index),
			FinishReason: finishReason,
			Delta: chat.MessageDelta{
				Role:             choice.Delta.Role,
				Content:          choice.Delta.Content,
				ReasoningContent: choice.Delta.ExtractReasoning(),
			},
		}

		if choice.Delta.FunctionCall != nil {
			response.Choices[i].Delta.FunctionCall = &tools.FunctionCall{
				Name:      choice.Delta.FunctionCall.Name,
				Arguments: choice.Delta.FunctionCall.Arguments,
			}
		}

		if len(choice.Delta.ToolCalls) > 0 {
			response.Choices[i].Delta.ToolCalls = make([]tools.ToolCall, len(choice.Delta.ToolCalls))
			for j, toolCall := range choice.Delta.ToolCalls {
				id := toolCall.ID
				index := int(toolCall.Index)
				if existing, ok := a.toolCalls[index]; ok && id == "" {
					id = existing
				} else if id != "" {
					a.toolCalls[index] = id
				}

				response.Choices[i].Delta.ToolCalls[j] = tools.ToolCall{
					ID:   id,
					Type: tools.ToolType(toolCall.Type),
					Function: tools.FunctionCall{
						Name:      toolCall.Function.Name,
						Arguments: toolCall.Function.Arguments,
					},
				}
			}
		}
	}

	if openaiResponse.Usage != nil {
		if a.trackUsage {
			usage := openaiResponse.Usage
			response.Usage = &chat.Usage{
				InputTokens:  usage.PromptTokens,
				OutputTokens: usage.CompletionTokens,
				ServiceTier:  a.serviceTier,
			}
			if details := usage.PromptTokensDetails; details != nil {
				response.Usage.CachedInputTokens = details.CachedTokens
				response.Usage.CacheWriteTokens = details.CacheWriteTokens
				response.Usage.InputTokens -= details.CachedTokens + details.CacheWriteTokens
			}
			if details := usage.CompletionTokensDetails; details != nil {
				response.Usage.ReasoningTokens = details.ReasoningTokens
			}
		}

		if len(openaiResponse.Choices) == 0 {
			finishReason := a.lastFinishReason
			if a.heldFinishReason != "" {
				finishReason = a.heldFinishReason
				a.heldFinishReason = ""
			}
			if finishReason == chat.FinishReasonNull || finishReason == "" {
				finishReason = chat.FinishReasonStop
			}
			response.Choices = append(response.Choices, chat.MessageStreamChoice{
				FinishReason: finishReason,
			})
		}
	}

	return response, nil
}

// Close closes the stream
func (a *StreamAdapter) Close() {
	_ = a.stream.Close()
}
