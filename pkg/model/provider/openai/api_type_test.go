package openai

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

func TestGetAPIType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		config   *latest.ModelConfig
		expected string
	}{
		{
			name:     "nil config",
			config:   nil,
			expected: "",
		},
		{
			name: "nil provider opts",
			config: &latest.ModelConfig{
				Provider:     "openai",
				Model:        "gpt-4o",
				ProviderOpts: nil,
			},
			expected: "",
		},
		{
			name: "empty provider opts",
			config: &latest.ModelConfig{
				Provider:     "openai",
				Model:        "gpt-4o",
				ProviderOpts: map[string]any{},
			},
			expected: "",
		},
		{
			name: "api_type set to openai_chatcompletions",
			config: &latest.ModelConfig{
				Provider: "custom_provider",
				Model:    "gpt-4o",
				ProviderOpts: map[string]any{
					"api_type": "openai_chatcompletions",
				},
			},
			expected: "openai_chatcompletions",
		},
		{
			name: "api_type set to openai_responses",
			config: &latest.ModelConfig{
				Provider: "custom_provider",
				Model:    "gpt-4o-pro",
				ProviderOpts: map[string]any{
					"api_type": "openai_responses",
				},
			},
			expected: "openai_responses",
		},
		{
			name: "api_type set to empty string",
			config: &latest.ModelConfig{
				Provider: "openai",
				Model:    "gpt-4o",
				ProviderOpts: map[string]any{
					"api_type": "",
				},
			},
			expected: "",
		},
		{
			name: "api_type set to non-string value (should be ignored)",
			config: &latest.ModelConfig{
				Provider: "openai",
				Model:    "gpt-4o",
				ProviderOpts: map[string]any{
					"api_type": 123,
				},
			},
			expected: "",
		},
		{
			name: "other provider opts present but no api_type",
			config: &latest.ModelConfig{
				Provider: "openai",
				Model:    "gpt-4o",
				ProviderOpts: map[string]any{
					"some_other_opt": "value",
				},
			},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := getAPIType(tt.config)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// writeSSEResponse writes a minimal valid SSE response for chat completions
func writeSSEResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)

	chunks := []map[string]any{
		{
			"id": "test", "object": "chat.completion.chunk", "model": "test",
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": "ok"}, "finish_reason": nil}},
		},
		{
			"id": "test", "object": "chat.completion.chunk", "model": "test",
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
		},
		{
			"id": "test", "object": "chat.completion.chunk", "model": "test",
			"choices": []map[string]any{}, "usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 1, "total_tokens": 6},
		},
	}
	for _, chunk := range chunks {
		data, _ := json.Marshal(chunk)
		_, _ = w.Write([]byte("data: " + string(data) + "\n\n"))
	}
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	flusher.Flush()
}

// TestCustomProvider_WithTokenKey verifies that when token_key is set,
// the Authorization header is sent with that token
func TestCustomProvider_WithTokenKey(t *testing.T) {
	t.Parallel()

	var (
		receivedAuth string
		mu           sync.Mutex
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		receivedAuth = r.Header.Get("Authorization")
		mu.Unlock()
		writeSSEResponse(w)
	}))
	defer server.Close()

	cfg := &latest.ModelConfig{
		Provider: "custom",
		Model:    "test",
		BaseURL:  server.URL,
		TokenKey: "MY_CUSTOM_TOKEN",
		ProviderOpts: map[string]any{
			"api_type": "openai_chatcompletions",
		},
	}

	env := environment.NewMapEnvProvider(map[string]string{
		"MY_CUSTOM_TOKEN": "secret-token-123",
	})

	client, err := NewClient(t.Context(), cfg, env)
	require.NoError(t, err)

	stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hi"}}, nil)
	require.NoError(t, err)
	defer stream.Close()

	// Drain stream
	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, "Bearer secret-token-123", receivedAuth, "Should use token from token_key env var")
}

// TestCustomProvider_WithoutTokenKey verifies that when base_url is set but
// token_key is not, requests are sent without a real auth token
func TestCustomProvider_WithoutTokenKey(t *testing.T) {
	t.Parallel()

	var (
		receivedAuth string
		mu           sync.Mutex
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		receivedAuth = r.Header.Get("Authorization")
		mu.Unlock()
		writeSSEResponse(w)
	}))
	defer server.Close()

	cfg := &latest.ModelConfig{
		Provider: "custom",
		Model:    "test",
		BaseURL:  server.URL,
		// No TokenKey - should send with empty/no token
		ProviderOpts: map[string]any{
			"api_type": "openai_chatcompletions",
		},
	}

	env := environment.NewNoEnvProvider()

	client, err := NewClient(t.Context(), cfg, env)
	require.NoError(t, err)

	stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hi"}}, nil)
	require.NoError(t, err)
	defer stream.Close()

	// Drain stream
	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}

	mu.Lock()
	defer mu.Unlock()
	// SDK omits the Authorization header entirely when no API key is set
	assert.Empty(t, receivedAuth, "Should not send an Authorization header when no token_key")
}
