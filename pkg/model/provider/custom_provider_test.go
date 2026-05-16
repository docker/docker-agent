package provider

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/tools"
)

// TestCustomProvider_WithProvidersOption tests the full flow using options.WithProviders
func TestCustomProvider_WithProvidersOption(t *testing.T) {
	t.Parallel()

	var (
		receivedAuth string
		receivedPath string
		mu           sync.Mutex
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		receivedAuth = r.Header.Get("Authorization")
		receivedPath = r.URL.Path
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)

		writeSSEChunk(w, map[string]any{
			"id": "test", "object": "chat.completion.chunk", "model": "gpt-4o",
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": "Hello"}, "finish_reason": "stop"}},
		})
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	defer server.Close()

	// Define custom providers (as would be done in config)
	customProviders := map[string]latest.ProviderConfig{
		"my_custom_gateway": {
			APIType:  "openai_chatcompletions",
			BaseURL:  server.URL,
			TokenKey: "MY_GATEWAY_TOKEN",
		},
	}

	// Model config references the custom provider by name
	// (base_url, token_key, and api_type NOT set - should come from provider)
	modelCfg := &latest.ModelConfig{
		Provider: "my_custom_gateway",
		Model:    "gpt-4o",
	}

	env := environment.NewMapEnvProvider(map[string]string{
		"MY_GATEWAY_TOKEN": "secret-from-provider",
	})

	// Create provider with WithProviders option (as teamloader does)
	provider, err := New(t.Context(), modelCfg, env, options.WithProviders(customProviders))
	require.NoError(t, err)

	stream, err := provider.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "Hi"}}, []tools.Tool{})
	require.NoError(t, err)
	defer stream.Close()

	drainStream(t, stream)

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, "Bearer secret-from-provider", receivedAuth, "Token should come from custom provider's token_key")
	assert.Equal(t, "/chat/completions", receivedPath, "Should use chat completions API")
}

// TestCustomProvider_RequestReachesServer tests that requests from a custom provider
// reach the configured server with the correct base URL, authorization, and token_key.
func TestCustomProvider_RequestReachesServer(t *testing.T) {
	t.Parallel()

	var (
		receivedAuth  string
		receivedPath  string
		receivedModel string
		mu            sync.Mutex
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		receivedAuth = r.Header.Get("Authorization")
		receivedPath = r.URL.Path
		mu.Unlock()

		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err == nil {
			if m, ok := payload["model"].(string); ok {
				mu.Lock()
				receivedModel = m
				mu.Unlock()
			}
		}

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)

		writeSSEChunk(w, map[string]any{
			"id": "test", "object": "chat.completion.chunk", "model": "gpt-4o",
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": "Hello"}, "finish_reason": nil}},
		})
		writeSSEChunk(w, map[string]any{
			"id": "test", "object": "chat.completion.chunk", "model": "gpt-4o",
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
		})
		writeSSEChunk(w, map[string]any{
			"id": "test", "object": "chat.completion.chunk", "model": "gpt-4o",
			"choices": []map[string]any{},
			"usage":   map[string]any{"prompt_tokens": 5, "completion_tokens": 1, "total_tokens": 6},
		})
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	defer server.Close()

	// Custom token key to verify token_key feature
	customTokenKey := "MY_CUSTOM_GATEWAY_TOKEN"
	expectedToken := "test-secret-key-123"

	modelCfg := &latest.ModelConfig{
		Provider: "my_custom_provider",
		Model:    "gpt-4o",
		BaseURL:  server.URL,
		TokenKey: customTokenKey,
		ProviderOpts: map[string]any{
			"api_type": "openai_chatcompletions",
		},
	}

	env := environment.NewMapEnvProvider(map[string]string{
		customTokenKey: expectedToken,
	})

	provider, err := New(t.Context(), modelCfg, env)
	require.NoError(t, err)

	stream, err := provider.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "Hello"}}, []tools.Tool{})
	require.NoError(t, err)
	defer stream.Close()

	drainStream(t, stream)

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, "Bearer "+expectedToken, receivedAuth, "Token from custom env var should be used")
	assert.Equal(t, "/chat/completions", receivedPath, "Request should go to /chat/completions")
	assert.Equal(t, "gpt-4o", receivedModel, "Model should be passed in the request")
}

// TestCustomProvider_ResponsesAPIType tests that api_type=openai_responses routes to /responses
func TestCustomProvider_ResponsesAPIType(t *testing.T) {
	t.Parallel()

	var (
		receivedPath string
		mu           sync.Mutex
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		receivedPath = r.URL.Path
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)

		// Responses API format
		events := []map[string]any{
			{"type": "response.created", "response_id": "resp_test"},
			{"type": "response.output_item.added", "item": map[string]any{"type": "message", "role": "assistant"}},
			{"type": "response.output_text.delta", "delta": map[string]any{"text": "Hello"}},
			{"type": "response.done", "status": "completed"},
		}
		for _, event := range events {
			eventJSON, _ := json.Marshal(event)
			_, _ = w.Write([]byte("event: " + event["type"].(string) + "\ndata: " + string(eventJSON) + "\n\n"))
			flusher.Flush()
		}
	}))
	defer server.Close()

	modelCfg := &latest.ModelConfig{
		Provider: "responses_provider",
		Model:    "gpt-4o-pro",
		BaseURL:  server.URL,
		TokenKey: "API_KEY",
		ProviderOpts: map[string]any{
			"api_type": "openai_responses",
		},
	}

	env := environment.NewMapEnvProvider(map[string]string{"API_KEY": "test"})

	provider, err := New(t.Context(), modelCfg, env)
	require.NoError(t, err)

	stream, err := provider.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "Hello"}}, []tools.Tool{})
	require.NoError(t, err)
	defer stream.Close()

	// Drain (may error due to mock format, but we only care about the path)
	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, "/responses", receivedPath, "api_type=openai_responses should route to /responses")
}

// TestCustomProvider_ChatCompletionsAPIType tests that api_type=openai_chatcompletions
// forces Chat Completions even for models that would normally use Responses API
func TestCustomProvider_ChatCompletionsAPIType(t *testing.T) {
	t.Parallel()

	var (
		receivedPath string
		mu           sync.Mutex
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		receivedPath = r.URL.Path
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)

		writeSSEChunk(w, map[string]any{
			"id": "test", "object": "chat.completion.chunk", "model": "gpt-4o-pro",
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": "Hello"}, "finish_reason": "stop"}},
		})
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	defer server.Close()

	// Model name with "-pro" suffix would normally trigger Responses API
	modelCfg := &latest.ModelConfig{
		Provider: "openai",
		Model:    "gpt-4o-pro",
		BaseURL:  server.URL,
		TokenKey: "OPENAI_API_KEY",
		ProviderOpts: map[string]any{
			"api_type": "openai_chatcompletions", // Force Chat Completions
		},
	}

	env := environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "test"})

	provider, err := New(t.Context(), modelCfg, env)
	require.NoError(t, err)

	stream, err := provider.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "Test"}}, []tools.Tool{})
	require.NoError(t, err)
	defer stream.Close()

	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, "/chat/completions", receivedPath, "api_type=openai_chatcompletions should force Chat Completions")
}

// TestCustomProvider_MissingAPIKey tests error handling for missing API key
func TestCustomProvider_MissingAPIKey(t *testing.T) {
	t.Parallel()

	modelCfg := &latest.ModelConfig{
		Provider: "custom_provider",
		Model:    "test-model",
		BaseURL:  "http://localhost:8888",
		TokenKey: "MISSING_API_KEY",
		ProviderOpts: map[string]any{
			"api_type": "openai_chatcompletions",
		},
	}

	env := environment.NewNoEnvProvider() // key not set

	_, err := New(t.Context(), modelCfg, env)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MISSING_API_KEY", "Error should mention the missing env var")
}

// TestApplyProviderDefaults_CustomProviders tests that custom provider defaults are applied lazily
func TestApplyProviderDefaults_CustomProviders(t *testing.T) {
	t.Parallel()

	customProviders := map[string]latest.ProviderConfig{
		"my_gateway": {
			APIType:  "openai_chatcompletions",
			BaseURL:  "https://api.example.com/v1",
			TokenKey: "MY_GATEWAY_KEY",
		},
	}

	tests := []struct {
		name             string
		modelCfg         *latest.ModelConfig
		expectedBaseURL  string
		expectedTokenKey string
		expectedAPIType  string
	}{
		{
			name: "applies all defaults from custom provider",
			modelCfg: &latest.ModelConfig{
				Provider: "my_gateway",
				Model:    "gpt-4o",
			},
			expectedBaseURL:  "https://api.example.com/v1",
			expectedTokenKey: "MY_GATEWAY_KEY",
			expectedAPIType:  "openai_chatcompletions",
		},
		{
			name: "model base_url takes precedence",
			modelCfg: &latest.ModelConfig{
				Provider: "my_gateway",
				Model:    "gpt-4o",
				BaseURL:  "https://override.example.com/v1",
			},
			expectedBaseURL:  "https://override.example.com/v1",
			expectedTokenKey: "MY_GATEWAY_KEY",
			expectedAPIType:  "openai_chatcompletions",
		},
		{
			name: "model token_key takes precedence",
			modelCfg: &latest.ModelConfig{
				Provider: "my_gateway",
				Model:    "gpt-4o",
				TokenKey: "OVERRIDE_KEY",
			},
			expectedBaseURL:  "https://api.example.com/v1",
			expectedTokenKey: "OVERRIDE_KEY",
			expectedAPIType:  "openai_chatcompletions",
		},
		{
			name: "model api_type takes precedence",
			modelCfg: &latest.ModelConfig{
				Provider: "my_gateway",
				Model:    "gpt-4o",
				ProviderOpts: map[string]any{
					"api_type": "openai_responses",
				},
			},
			expectedBaseURL:  "https://api.example.com/v1",
			expectedTokenKey: "MY_GATEWAY_KEY",
			expectedAPIType:  "openai_responses",
		},
		{
			name: "unknown provider returns unchanged config",
			modelCfg: &latest.ModelConfig{
				Provider: "unknown_provider",
				Model:    "test-model",
			},
			expectedBaseURL:  "",
			expectedTokenKey: "",
			expectedAPIType:  "", // No api_type set
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := applyProviderDefaults(tt.modelCfg, customProviders)

			assert.Equal(t, tt.expectedBaseURL, result.BaseURL)
			assert.Equal(t, tt.expectedTokenKey, result.TokenKey)

			if tt.expectedAPIType != "" {
				assert.Equal(t, tt.expectedAPIType, result.ProviderOpts["api_type"])
			} else if result.ProviderOpts != nil {
				// ProviderOpts exists but api_type should not be set
				_, hasAPIType := result.ProviderOpts["api_type"]
				assert.False(t, hasAPIType, "api_type should not be set")
			}
		})
	}
}

// TestApplyProviderDefaults_DefaultsAPIType tests that empty api_type defaults to openai_chatcompletions
func TestApplyProviderDefaults_DefaultsAPIType(t *testing.T) {
	t.Parallel()

	customProviders := map[string]latest.ProviderConfig{
		"no_api_type": {
			BaseURL:  "https://api.example.com/v1",
			TokenKey: "KEY",
			// APIType is empty
		},
	}

	modelCfg := &latest.ModelConfig{
		Provider: "no_api_type",
		Model:    "test",
	}

	result := applyProviderDefaults(modelCfg, customProviders)
	assert.Equal(t, "openai_chatcompletions", result.ProviderOpts["api_type"])
}

// TestResolveProviderTypeFromConfig tests the provider type resolution logic
func TestResolveProviderTypeFromConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		config   *latest.ModelConfig
		expected string
	}{
		{
			name: "api_type from ProviderOpts takes priority",
			config: &latest.ModelConfig{
				Provider:     "openai",
				ProviderOpts: map[string]any{"api_type": "openai_responses"},
			},
			expected: "openai_responses",
		},
		{
			name: "built-in alias takes second priority",
			config: &latest.ModelConfig{
				Provider: "mistral", // Has alias with APIType: "openai"
			},
			expected: "openai",
		},
		{
			name: "provider name is fallback",
			config: &latest.ModelConfig{
				Provider: "anthropic",
			},
			expected: "anthropic",
		},
		{
			name: "custom provider with api_type",
			config: &latest.ModelConfig{
				Provider:     "my_custom_provider",
				ProviderOpts: map[string]any{"api_type": "openai_chatcompletions"},
			},
			expected: "openai_chatcompletions",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, resolveProviderType(tt.config))
		})
	}
}

// Helper functions

func writeSSEChunk(w http.ResponseWriter, data map[string]any) {
	jsonData, _ := json.Marshal(data)
	_, _ = w.Write([]byte("data: " + string(jsonData) + "\n\n"))
}

func drainStream(t *testing.T, stream chat.MessageStream) {
	t.Helper()
	for {
		_, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			return
		}
	}
}
