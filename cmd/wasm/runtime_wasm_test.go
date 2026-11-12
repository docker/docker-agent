//go:build js && wasm

package main

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestToolCallPreservesProviderToolCallID(t *testing.T) {
	for _, providerID := range []string{"", "gemini-call"} {
		t.Run("providerID="+providerID, func(t *testing.T) {
			stream := &wasmTestStream{}
			for _, call := range []tools.ToolCall{
				{ID: "local-call", Type: "function", Function: tools.FunctionCall{Name: "echo"}},
				{ID: "local-call", ProviderID: providerID, Function: tools.FunctionCall{Arguments: `{"text":`}},
				{ID: "local-call", Function: tools.FunctionCall{Arguments: `"Paris"}`}},
			} {
				stream.responses = append(stream.responses, choice(chat.MessageDelta{ToolCalls: []tools.ToolCall{call}}))
			}
			stream.responses = append(stream.responses, stop(chat.FinishReasonToolCalls, 10, 5))
			model := newScriptedModel("mock/root", func(context.Context) (chat.MessageStream, error) {
				return stream, nil
			}, textTurn("done"))
			echo := &echoToolSet{}
			s := openTestSession(t, testHost(echo, map[string]provider.Provider{"root": model}), sessionOptions{YAML: echoAgentYAML, AutoApprove: true})

			_, err := s.send("echo Paris", func(context.Context, map[string]any) {})
			require.NoError(t, err)
			assert.True(t, stream.closed)
			assert.Equal(t, 1, echo.callCount())
			require.Equal(t, 2, model.callCount())
			messages := model.lastCall()
			require.Len(t, messages, 4)
			assert.Equal(t, chat.MessageRoleAssistant, messages[2].Role)
			require.Len(t, messages[2].ToolCalls, 1)
			call := messages[2].ToolCalls[0]
			assert.Equal(t, "local-call", call.ID)
			assert.Equal(t, providerID, call.ProviderID)
			assert.Equal(t, tools.ToolType("function"), call.Type)
			assert.Equal(t, "echo", call.Function.Name)
			assert.JSONEq(t, `{"text":"Paris"}`, call.Function.Arguments)
		})
	}
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
