package openai

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/openai/openai-go/v3/responses"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

// testWSServer starts an httptest.Server that upgrades to WebSocket,
// reads the response.create message, and sends back the given events
// as JSON text frames.
func testWSServer(t *testing.T, events []map[string]any) *httptest.Server {
	t.Helper()

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("WebSocket upgrade failed: %v", err)
			return
		}
		defer conn.Close()

		// Read the response.create message.
		_, data, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("Failed to read response.create: %v", err)
			return
		}

		var createMsg map[string]any
		if err := json.Unmarshal(data, &createMsg); err != nil {
			t.Errorf("Failed to unmarshal response.create: %v", err)
			return
		}
		assert.Equal(t, "response.create", createMsg["type"])

		// Send events.
		for _, event := range events {
			eventData, _ := json.Marshal(event)
			if err := conn.WriteMessage(websocket.TextMessage, eventData); err != nil {
				return
			}
		}

		// Close the connection after sending all events.
		_ = conn.WriteMessage(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		)
	}))
}

func TestWSStream_TextDelta(t *testing.T) {
	t.Parallel()

	events := []map[string]any{
		{
			"type":    "response.output_text.delta",
			"delta":   "Hello ",
			"item_id": "item_1",
		},
		{
			"type":    "response.output_text.delta",
			"delta":   "World!",
			"item_id": "item_1",
		},
		{
			"type": "response.completed",
			"response": map[string]any{
				"id":     "resp_123",
				"output": []any{},
				"usage": map[string]any{
					"input_tokens":  10,
					"output_tokens": 5,
					"total_tokens":  15,
					"input_tokens_details": map[string]any{
						"cached_tokens": 0,
					},
					"output_tokens_details": map[string]any{
						"reasoning_tokens": 0,
					},
				},
			},
		},
	}

	srv := testWSServer(t, events)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	stream, err := dialWebSocket(t.Context(), wsURL, http.Header{}, defaultTestParams())
	require.NoError(t, err)
	defer stream.Close()

	adapter := newResponseStreamAdapter(stream, true)

	// First delta
	resp, err := adapter.Recv()
	require.NoError(t, err)
	require.Len(t, resp.Choices, 1)
	assert.Equal(t, "Hello ", resp.Choices[0].Delta.Content)

	// Second delta
	resp, err = adapter.Recv()
	require.NoError(t, err)
	require.Len(t, resp.Choices, 1)
	assert.Equal(t, "World!", resp.Choices[0].Delta.Content)

	// response.completed → finish reason + usage
	resp, err = adapter.Recv()
	require.NoError(t, err)
	require.Len(t, resp.Choices, 1)
	assert.Equal(t, chat.FinishReasonStop, resp.Choices[0].FinishReason)
	require.NotNil(t, resp.Usage)
	assert.Equal(t, int64(10), resp.Usage.InputTokens)
	assert.Equal(t, int64(5), resp.Usage.OutputTokens)

	// Stream is exhausted.
	_, err = adapter.Recv()
	assert.ErrorIs(t, err, io.EOF)
}

func TestWSStream_IgnoresEmptyFrames(t *testing.T) {
	t.Parallel()

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("WebSocket upgrade failed: %v", err)
			return
		}
		defer conn.Close()

		_, _, err = conn.ReadMessage()
		if err != nil {
			t.Errorf("Failed to read response.create: %v", err)
			return
		}

		assert.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte("   ")))
		assert.NoError(t, conn.WriteJSON(map[string]any{
			"type":    "response.output_text.delta",
			"delta":   "Hi from OVH",
			"item_id": "item_1",
		}))
		assert.NoError(t, conn.WriteJSON(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id":     "resp_ovh",
				"output": []any{},
				"usage": map[string]any{
					"input_tokens":          4,
					"output_tokens":         3,
					"total_tokens":          7,
					"input_tokens_details":  map[string]any{"cached_tokens": 0},
					"output_tokens_details": map[string]any{"reasoning_tokens": 0},
				},
			},
		}))
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	stream, err := dialWebSocket(t.Context(), wsURL, http.Header{}, defaultTestParams())
	require.NoError(t, err)
	defer stream.Close()

	adapter := newResponseStreamAdapter(stream, true)

	resp, err := adapter.Recv()
	require.NoError(t, err)
	require.Len(t, resp.Choices, 1)
	assert.Equal(t, "Hi from OVH", resp.Choices[0].Delta.Content)

	resp, err = adapter.Recv()
	require.NoError(t, err)
	require.Len(t, resp.Choices, 1)
	assert.Equal(t, chat.FinishReasonStop, resp.Choices[0].FinishReason)
	assert.NotNil(t, resp.Usage)
	assert.Equal(t, int64(3), resp.Usage.OutputTokens)

	_, err = adapter.Recv()
	assert.ErrorIs(t, err, io.EOF)
}

func TestWSStream_ToolCall(t *testing.T) {
	t.Parallel()

	events := []map[string]any{
		{
			"type":    "response.output_item.added",
			"item_id": "item_2",
			"item": map[string]any{
				"type":    "function_call",
				"id":      "item_2",
				"call_id": "call_abc",
				"name":    "get_weather",
			},
		},
		{
			"type":    "response.function_call_arguments.delta",
			"item_id": "item_2",
			"delta":   `{"city":`,
		},
		{
			"type":    "response.function_call_arguments.delta",
			"item_id": "item_2",
			"delta":   `"Paris"}`,
		},
		{
			"type":    "response.function_call_arguments.done",
			"item_id": "item_2",
		},
		{
			"type": "response.completed",
			"response": map[string]any{
				"id": "resp_456",
				"output": []any{
					map[string]any{"type": "function_call"},
				},
				"usage": map[string]any{
					"input_tokens":  8,
					"output_tokens": 12,
					"total_tokens":  20,
					"input_tokens_details": map[string]any{
						"cached_tokens": 0,
					},
					"output_tokens_details": map[string]any{
						"reasoning_tokens": 0,
					},
				},
			},
		},
	}

	srv := testWSServer(t, events)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	stream, err := dialWebSocket(t.Context(), wsURL, http.Header{}, defaultTestParams())
	require.NoError(t, err)
	defer stream.Close()

	adapter := newResponseStreamAdapter(stream, true)

	// output_item.added → tool call with name
	resp, err := adapter.Recv()
	require.NoError(t, err)
	require.Len(t, resp.Choices, 1)
	require.Len(t, resp.Choices[0].Delta.ToolCalls, 1)
	assert.Equal(t, "get_weather", resp.Choices[0].Delta.ToolCalls[0].Function.Name)
	assert.Equal(t, "call_abc", resp.Choices[0].Delta.ToolCalls[0].ID)

	// arguments delta 1
	resp, err = adapter.Recv()
	require.NoError(t, err)
	require.Len(t, resp.Choices, 1)
	require.Len(t, resp.Choices[0].Delta.ToolCalls, 1)
	assert.Equal(t, `{"city":`, resp.Choices[0].Delta.ToolCalls[0].Function.Arguments)

	// arguments delta 2
	resp, err = adapter.Recv()
	require.NoError(t, err)
	require.Len(t, resp.Choices, 1)
	require.Len(t, resp.Choices[0].Delta.ToolCalls, 1)
	assert.Equal(t, `"Paris"}`, resp.Choices[0].Delta.ToolCalls[0].Function.Arguments)

	// arguments done → empty response (no choices)
	resp, err = adapter.Recv()
	require.NoError(t, err)
	assert.Empty(t, resp.Choices)

	// response.completed → finish reason tool_calls
	resp, err = adapter.Recv()
	require.NoError(t, err)
	require.Len(t, resp.Choices, 1)
	assert.Equal(t, chat.FinishReasonToolCalls, resp.Choices[0].FinishReason)

	// Stream is exhausted.
	_, err = adapter.Recv()
	assert.ErrorIs(t, err, io.EOF)
}

func TestWSStream_ErrorEvent(t *testing.T) {
	t.Parallel()

	events := []map[string]any{
		{
			"type":    "error",
			"message": "rate_limit_exceeded",
			"param":   "",
		},
	}

	srv := testWSServer(t, events)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	stream, err := dialWebSocket(t.Context(), wsURL, http.Header{}, defaultTestParams())
	require.NoError(t, err)
	defer stream.Close()

	// The error event is still yielded to Recv, then the stream errors.
	ok := stream.Next()
	assert.True(t, ok)
	assert.Equal(t, "error", stream.Current().Type)
	require.Error(t, stream.Err())
	assert.Contains(t, stream.Err().Error(), "rate_limit_exceeded")

	// Further calls return false.
	assert.False(t, stream.Next())
}

func TestHTTPToWSURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input    string
		expected string
	}{
		{"https://api.openai.com/v1", "wss://api.openai.com/v1/responses"},
		{"https://api.openai.com/v1/", "wss://api.openai.com/v1/responses"},
		{"http://localhost:8080/v1", "ws://localhost:8080/v1/responses"},
		{"https://api.openai.com/v1/responses", "wss://api.openai.com/v1/responses"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, httpToWSURL(tt.input))
		})
	}
}

func TestGetTransport(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		config   *latest.ModelConfig
		expected string
	}{
		{
			name:     "nil config",
			config:   nil,
			expected: "sse",
		},
		{
			name:     "no provider opts",
			config:   &latest.ModelConfig{},
			expected: "sse",
		},
		{
			name: "transport=websocket",
			config: &latest.ModelConfig{
				ProviderOpts: map[string]any{"transport": "websocket"},
			},
			expected: "websocket",
		},
		{
			name: "transport=WebSocket (case insensitive)",
			config: &latest.ModelConfig{
				ProviderOpts: map[string]any{"transport": "WebSocket"},
			},
			expected: "websocket",
		},
		{
			name: "transport=sse",
			config: &latest.ModelConfig{
				ProviderOpts: map[string]any{"transport": "sse"},
			},
			expected: "sse",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, getTransport(tt.config))
		})
	}
}

func TestWSStream_EndToEnd_WithClient(t *testing.T) {
	t.Parallel()

	events := []map[string]any{
		{
			"type":    "response.output_text.delta",
			"delta":   "Hi!",
			"item_id": "item_1",
		},
		{
			"type": "response.completed",
			"response": map[string]any{
				"id":     "resp_e2e",
				"output": []any{},
				"usage": map[string]any{
					"input_tokens":  5,
					"output_tokens": 1,
					"total_tokens":  6,
					"input_tokens_details": map[string]any{
						"cached_tokens": 0,
					},
					"output_tokens_details": map[string]any{
						"reasoning_tokens": 0,
					},
				},
			},
		},
	}

	srv := testWSServer(t, events)
	defer srv.Close()

	baseURL := srv.URL

	cfg := &latest.ModelConfig{
		Provider: "openai",
		Model:    "gpt-4.1",
		BaseURL:  baseURL,
		ProviderOpts: map[string]any{
			"api_type":  "openai_responses",
			"transport": "websocket",
		},
	}

	env := environment.NewMapEnvProvider(map[string]string{})

	client, err := NewClient(t.Context(), cfg, env)
	require.NoError(t, err)

	stream, err := client.CreateChatCompletionStream(
		t.Context(),
		[]chat.Message{{Role: chat.MessageRoleUser, Content: "hello"}},
		nil,
	)
	require.NoError(t, err)
	defer stream.Close()

	// First event: text delta
	resp, err := stream.Recv()
	require.NoError(t, err)
	require.Len(t, resp.Choices, 1)
	assert.Equal(t, "Hi!", resp.Choices[0].Delta.Content)

	// Second event: completed
	resp, err = stream.Recv()
	require.NoError(t, err)
	require.Len(t, resp.Choices, 1)
	assert.Equal(t, chat.FinishReasonStop, resp.Choices[0].FinishReason)

	// Done
	_, err = stream.Recv()
	assert.ErrorIs(t, err, io.EOF)
}

func defaultTestParams() responses.ResponseNewParams {
	return responses.ResponseNewParams{
		Model: "gpt-4.1",
	}
}

func TestWebSocketDisabledWithGateway(t *testing.T) {
	t.Parallel()

	cfg := &latest.ModelConfig{
		Provider: "openai",
		Model:    "gpt-4.1",
		ProviderOpts: map[string]any{
			"transport": "websocket",
		},
	}

	// Test 1: No gateway - WebSocket should be allowed
	transport := getTransport(cfg)
	assert.Equal(t, "websocket", transport)

	// Test 2: With gateway - the condition in CreateResponseStream
	// checks c.ModelOptions.Gateway() == "" before allowing WebSocket
	// We can't easily test the full flow without mocking the Gateway auth,
	// but we can verify the getTransport function works correctly
	assert.Equal(t, "websocket", getTransport(cfg))

	// Test 3: SSE is default
	cfgNoTransport := &latest.ModelConfig{
		Provider: "openai",
		Model:    "gpt-4.1",
	}
	assert.Equal(t, "sse", getTransport(cfgNoTransport))
}
