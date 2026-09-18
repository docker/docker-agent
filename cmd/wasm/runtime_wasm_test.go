//go:build js && wasm

package main

import (
	"context"
	"io"
	"syscall/js"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestStreamCompletion_PreservesProviderToolCallID(t *testing.T) {
	for _, providerID := range []string{"", "gemini-call"} {
		t.Run("providerID="+providerID, func(t *testing.T) {
			stream := &wasmTestStream{}
			for _, call := range []tools.ToolCall{
				{ID: "local-call", Type: "function", Function: tools.FunctionCall{Name: "lookup"}},
				{ID: "local-call", ProviderID: providerID, Function: tools.FunctionCall{Arguments: `{"city":`}},
				{ID: "local-call", Function: tools.FunctionCall{Arguments: `"Paris"}`}},
			} {
				stream.responses = append(stream.responses, chat.MessageStreamResponse{
					Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{ToolCalls: []tools.ToolCall{call}}}},
				})
			}
			rt := &wasmRuntime{onEvent: js.Undefined()}
			result, err := rt.streamCompletion(t.Context(), &wasmTestProvider{stream: stream}, nil, nil)
			require.NoError(t, err)
			assert.True(t, stream.closed)
			require.Len(t, result.toolCalls, 1)
			call := result.toolCalls[0]
			assert.Equal(t, "local-call", call.ID)
			assert.Equal(t, providerID, call.ProviderID)
			assert.Equal(t, tools.ToolType("function"), call.Type)
			assert.Equal(t, "lookup", call.Function.Name)
			assert.JSONEq(t, `{"city":"Paris"}`, call.Function.Arguments)
		})
	}
}

type wasmTestProvider struct {
	provider.Provider

	stream chat.MessageStream
}

func (p *wasmTestProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return p.stream, nil
}

type wasmTestStream struct {
	responses []chat.MessageStreamResponse
	closed    bool
}

func (s *wasmTestStream) Recv() (chat.MessageStreamResponse, error) {
	if len(s.responses) == 0 {
		return chat.MessageStreamResponse{}, io.EOF
	}
	response := s.responses[0]
	s.responses = s.responses[1:]
	return response, nil
}

func (s *wasmTestStream) Close() { s.closed = true }
