package oaistream

/*
This is a shared adapter for OpenAI-compatible streams.
*/

import (
	"encoding/json"
	"io"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/ssestream"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/tools"
)

// StreamAdapter adapts the OpenAI stream to our interface
type StreamAdapter struct {
	stream           *ssestream.Stream[openai.ChatCompletionChunk]
	lastFinishReason chat.FinishReason
	toolCalls        map[int]string
	trackUsage       bool
}

func NewStreamAdapter(stream *ssestream.Stream[openai.ChatCompletionChunk], trackUsage bool) *StreamAdapter {
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
		return chat.MessageStreamResponse{}, io.EOF
	}

	openaiResponse := a.stream.Current()

	// Convert the OpenAI response to our generic format
	response := chat.MessageStreamResponse{
		ID:      openaiResponse.ID,
		Object:  string(openaiResponse.Object),
		Created: openaiResponse.Created,
		Model:   openaiResponse.Model,
		Choices: make([]chat.MessageStreamChoice, len(openaiResponse.Choices)),
	}

	// Convert the choices
	for i := range openaiResponse.Choices {
		choice := &openaiResponse.Choices[i]

		finishReasonStr := choice.FinishReason
		if a.trackUsage && (finishReasonStr == "stop" || finishReasonStr == "length") {
			finishReasonStr = ""
		}

		finishReason := chat.FinishReason(finishReasonStr)
		// Track the finish reason for when we get usage info
		if finishReason != chat.FinishReasonNull && finishReason != "" {
			a.lastFinishReason = finishReason
		}

		// Extract reasoning text from ExtraFields since the OpenAI SDK does
		// not have a dedicated field for it. OpenAI-compatible providers
		// carry thinking tokens under different keys: DMR/DeepSeek use
		// "reasoning_content", while Qwen3 thinking mode (e.g. via OVHcloud
		// AI Endpoints), OpenRouter and several vLLM/SGLang builds use
		// "reasoning". Without handling "reasoning", a Qwen3 turn that only
		// reasons and then stops is dropped entirely and the user sees an
		// empty reply. See https://github.com/docker/docker-agent/issues/3145.
		var reasoningContent string
		for _, key := range []string{"reasoning_content", "reasoning"} {
			ef, ok := choice.Delta.JSON.ExtraFields[key]
			if !ok || ef.Raw() == "" {
				continue
			}
			// ef.Raw() returns the raw JSON value (e.g. `"some text"`), so we
			// unmarshal it to get the plain Go string. Some providers send a
			// non-string "reasoning" (e.g. an object); ignore those and keep
			// looking rather than surfacing a partial value.
			if err := json.Unmarshal([]byte(ef.Raw()), &reasoningContent); err != nil {
				reasoningContent = ""
				continue
			}
			if reasoningContent != "" {
				break
			}
		}

		response.Choices[i] = chat.MessageStreamChoice{
			Index:        int(choice.Index),
			FinishReason: finishReason,
			Delta: chat.MessageDelta{
				Role:             choice.Delta.Role,
				Content:          choice.Delta.Content,
				ReasoningContent: reasoningContent,
			},
		}

		// Convert function call if present
		if choice.Delta.JSON.FunctionCall.Valid() {
			funcCall := choice.Delta.FunctionCall
			response.Choices[i].Delta.FunctionCall = &tools.FunctionCall{
				Name:      funcCall.Name,
				Arguments: funcCall.Arguments,
			}
		}

		// Convert tool calls if present
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

	// Check if Usage field is present using the JSON metadata
	if openaiResponse.JSON.Usage.Valid() {
		if a.trackUsage {
			usage := openaiResponse.Usage
			response.Usage = &chat.Usage{
				InputTokens:  usage.PromptTokens,
				OutputTokens: usage.CompletionTokens,
			}
			if usage.JSON.PromptTokensDetails.Valid() {
				response.Usage.CachedInputTokens = usage.PromptTokensDetails.CachedTokens
				response.Usage.InputTokens -= usage.PromptTokensDetails.CachedTokens
			}
			if usage.JSON.CompletionTokensDetails.Valid() {
				response.Usage.ReasoningTokens = usage.CompletionTokensDetails.ReasoningTokens
			}
		}

		// Use the tracked finish reason instead of hardcoding stop
		finishReason := a.lastFinishReason
		if finishReason == chat.FinishReasonNull || finishReason == "" {
			finishReason = chat.FinishReasonStop
		}
		// OPENAI returns the usage without a finish reason or a choice, so we fake it here
		// and create a new choice for the last event in the stream
		if len(openaiResponse.Choices) == 0 {
			response.Choices = append(response.Choices, chat.MessageStreamChoice{
				FinishReason: finishReason,
			})
		} else {
			// Other openai-compatible providers DO return a choice with finish reason...
			response.Choices[0].FinishReason = finishReason
		}
		if finishReason == chat.FinishReasonStop {
			return response, nil
		}
	}

	return response, nil
}

// Close closes the stream
func (a *StreamAdapter) Close() {
	_ = a.stream.Close()
}
