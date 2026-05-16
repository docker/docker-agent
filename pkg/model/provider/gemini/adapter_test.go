package gemini

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/genai"

	"github.com/docker/docker-agent/pkg/chat"
)

func TestStreamAdapter_FunctionCalls(t *testing.T) {
	t.Run("function calls in final message", func(t *testing.T) {
		mockResp := &genai.GenerateContentResponse{
			Candidates: []*genai.Candidate{
				{
					Content: &genai.Content{
						Parts: []*genai.Part{
							{
								FunctionCall: &genai.FunctionCall{
									Name: "test_function",
									Args: map[string]any{"param": "value"},
								},
							},
						},
					},
				},
			},
		}

		// Simulate the iterator behavior
		iter := func(fn func(*genai.GenerateContentResponse, error) bool) {
			// Send the response with function call
			fn(mockResp, nil)
		}

		adapter := NewStreamAdapter(iter, "test-model", true)

		// Read the response
		resp, err := adapter.Recv()
		require.NoError(t, err)

		// Should have tool calls
		require.NotEmpty(t, resp.Choices[0].Delta.ToolCalls)

		// Read the final message
		finalResp, err := adapter.Recv()
		require.NoError(t, err)

		// Should have finish reason tool_calls
		require.Equal(t, chat.FinishReasonToolCalls, finalResp.Choices[0].FinishReason)

		// Should NOT include tool calls in final message (to avoid duplication)
		require.Empty(t, finalResp.Choices[0].Delta.ToolCalls)
	})
}
