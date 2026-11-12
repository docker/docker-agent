//go:build !js && !docker_agent_no_openai

package provider

import (
	"encoding/json"
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

// captureNamedCustomProviderRequestBody starts a mock server that records the
// last request body it received and replies with a minimal SSE stream in the
// given API's shape ("openai_responses" or "openai_chatcompletions").
func captureNamedCustomProviderRequestBody(t *testing.T, apiType string) (server *httptest.Server, body func() []byte) {
	t.Helper()

	var (
		received []byte
		mu       sync.Mutex
	)
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		received = b
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)

		if apiType == "openai_responses" {
			events := []map[string]any{
				{"type": "response.created", "response_id": "resp_test"},
				{"type": "response.done", "status": "completed"},
			}
			for _, event := range events {
				eventJSON, _ := json.Marshal(event)
				_, _ = w.Write([]byte("event: " + event["type"].(string) + "\ndata: " + string(eventJSON) + "\n\n"))
				flusher.Flush()
			}
			return
		}

		writeSSEChunk(w, map[string]any{
			"id": "test", "object": "chat.completion.chunk", "model": "gpt-5.6",
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": "Hello"}, "finish_reason": "stop"}},
		})
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	t.Cleanup(server.Close)
	return server, func() []byte {
		mu.Lock()
		defer mu.Unlock()
		return received
	}
}

// TestNamedCustomOpenAIProvider_ChatCompletions_NoThinking_SendsNoneEffort is
// the end-to-end regression test for the named-custom-OpenAI-provider case: a
// named custom provider (providers: section) that omits the underlying
// `provider:` field defaults to the OpenAI protocol/vendor (see
// defaultOpenAIAPIType and isOpenAIVendor in defaults.go), so it must get
// gpt-5.6's real reasoning_effort="none" from the NoThinking() request-option
// path, not the generic "low" fallback used for aliases that front a
// different vendor (xai, mistral, ...). Exercises the full pipeline:
// createDirectProvider computing isOpenAIVendor and passing
// options.WithOpenAIVendor to the openai client.
func TestNamedCustomOpenAIProvider_ChatCompletions_NoThinking_SendsNoneEffort(t *testing.T) {
	t.Parallel()

	server, body := captureNamedCustomProviderRequestBody(t, "openai_chatcompletions")

	customProviders := map[string]latest.ProviderConfig{
		"my_openai": {
			BaseURL:  server.URL,
			TokenKey: "MY_OPENAI_TOKEN",
			APIType:  "openai_chatcompletions",
		},
	}
	modelCfg := &latest.ModelConfig{Provider: "my_openai", Model: "gpt-5.6"}
	env := environment.NewMapEnvProvider(map[string]string{"MY_OPENAI_TOKEN": "secret"})

	provider, err := fullTestRegistry().New(t.Context(), modelCfg, env,
		options.WithProviders(customProviders), options.WithNoThinking())
	require.NoError(t, err)

	stream, err := provider.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "Hi"}}, nil)
	require.NoError(t, err)
	defer stream.Close()
	drainStream(t, stream)

	var req struct {
		ReasoningEffort string `json:"reasoning_effort"`
	}
	require.NoError(t, json.Unmarshal(body(), &req))
	assert.Equal(t, "none", req.ReasoningEffort)
}

// TestNamedCustomOpenAIProvider_Responses_NoThinking_SendsNoneEffort is the
// Responses-API counterpart of
// TestNamedCustomOpenAIProvider_ChatCompletions_NoThinking_SendsNoneEffort.
func TestNamedCustomOpenAIProvider_Responses_NoThinking_SendsNoneEffort(t *testing.T) {
	t.Parallel()

	server, body := captureNamedCustomProviderRequestBody(t, "openai_responses")

	customProviders := map[string]latest.ProviderConfig{
		"my_openai": {
			BaseURL:  server.URL,
			TokenKey: "MY_OPENAI_TOKEN",
			APIType:  "openai_responses",
		},
	}
	modelCfg := &latest.ModelConfig{Provider: "my_openai", Model: "gpt-5.6"}
	env := environment.NewMapEnvProvider(map[string]string{"MY_OPENAI_TOKEN": "secret"})

	provider, err := fullTestRegistry().New(t.Context(), modelCfg, env,
		options.WithProviders(customProviders), options.WithNoThinking())
	require.NoError(t, err)

	stream, err := provider.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "Hi"}}, nil)
	require.NoError(t, err)
	defer stream.Close()
	drainStream(t, stream)

	var req struct {
		Reasoning struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
	}
	require.NoError(t, json.Unmarshal(body(), &req))
	assert.Equal(t, "none", req.Reasoning.Effort)
}

// TestNamedCustomOpenAIProvider_UnrecognizedAlias_KeepsLowFallback is the
// negative counterpart: a *known* built-in alias that merely speaks the
// OpenAI wire protocol for a different vendor (xai, mistral) must never gain
// the "none" effort just because its api_type resolves to an OpenAI dialect
// through the alias table.
func TestNamedCustomOpenAIProvider_UnrecognizedAlias_KeepsLowFallback(t *testing.T) {
	t.Parallel()

	tests := []struct {
		provider string
		tokenKey string
	}{
		{"xai", "XAI_API_KEY"},
		{"mistral", "MISTRAL_API_KEY"},
	}
	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			t.Parallel()

			server, body := captureNamedCustomProviderRequestBody(t, "openai_chatcompletions")

			modelCfg := &latest.ModelConfig{
				Provider: tt.provider,
				Model:    "gpt-5.6",
				BaseURL:  server.URL,
				ProviderOpts: map[string]any{
					"api_type": "openai_chatcompletions",
				},
			}
			env := environment.NewMapEnvProvider(map[string]string{tt.tokenKey: "secret"})

			p, err := fullTestRegistry().New(t.Context(), modelCfg, env, options.WithNoThinking())
			require.NoError(t, err)

			stream, err := p.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "Hi"}}, nil)
			require.NoError(t, err)
			defer stream.Close()
			drainStream(t, stream)

			var req struct {
				ReasoningEffort string `json:"reasoning_effort"`
			}
			require.NoError(t, json.Unmarshal(body(), &req))
			assert.Equal(t, "low", req.ReasoningEffort)
		})
	}
}

// TestNamedCustomOpenAIProvider_SpoofedProviderOptsCannotForceNone is the
// adversarial regression test for the full registry/factory pipeline: a user
// setting provider_opts.openai_vendor: true on a *known* non-OpenAI alias
// (xai, mistral) must not be able to force reasoning_effort="none". The
// value must still fall back to "low", proving the removed public
// ProviderOpts marker plays no role even when a user tries to set it
// directly through YAML-equivalent config.
func TestNamedCustomOpenAIProvider_SpoofedProviderOptsCannotForceNone(t *testing.T) {
	t.Parallel()

	tests := []struct {
		provider string
		tokenKey string
	}{
		{"xai", "XAI_API_KEY"},
		{"mistral", "MISTRAL_API_KEY"},
	}
	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			t.Parallel()

			server, body := captureNamedCustomProviderRequestBody(t, "openai_chatcompletions")

			modelCfg := &latest.ModelConfig{
				Provider: tt.provider,
				Model:    "gpt-5.6",
				BaseURL:  server.URL,
				ProviderOpts: map[string]any{
					"api_type":      "openai_chatcompletions",
					"openai_vendor": true, // spoofed; must be ignored
				},
			}
			env := environment.NewMapEnvProvider(map[string]string{tt.tokenKey: "secret"})

			p, err := fullTestRegistry().New(t.Context(), modelCfg, env, options.WithNoThinking())
			require.NoError(t, err)

			stream, err := p.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "Hi"}}, nil)
			require.NoError(t, err)
			defer stream.Close()
			drainStream(t, stream)

			var req struct {
				ReasoningEffort string `json:"reasoning_effort"`
			}
			require.NoError(t, json.Unmarshal(body(), &req))
			assert.Equal(t, "low", req.ReasoningEffort)
		})
	}
}

// TestNamedCustomOpenAIProvider_SpoofedProviderOptsCannotSuppressNone is the
// mirror-image adversarial test: a user setting provider_opts.openai_vendor:
// false on a named custom OpenAI provider (providers: section, no explicit
// `provider:` override) must not suppress reasoning_effort="none" \u2014 the
// factory's own isOpenAIVendor resolution (threaded via
// options.WithOpenAIVendor) is what decides, not the public map.
func TestNamedCustomOpenAIProvider_SpoofedProviderOptsCannotSuppressNone(t *testing.T) {
	t.Parallel()

	server, body := captureNamedCustomProviderRequestBody(t, "openai_chatcompletions")

	customProviders := map[string]latest.ProviderConfig{
		"my_openai": {
			BaseURL:  server.URL,
			TokenKey: "MY_OPENAI_TOKEN",
			APIType:  "openai_chatcompletions",
		},
	}
	modelCfg := &latest.ModelConfig{
		Provider: "my_openai",
		Model:    "gpt-5.6",
		ProviderOpts: map[string]any{
			"openai_vendor": false, // spoofed; must not suppress "none"
		},
	}
	env := environment.NewMapEnvProvider(map[string]string{"MY_OPENAI_TOKEN": "secret"})

	provider, err := fullTestRegistry().New(t.Context(), modelCfg, env,
		options.WithProviders(customProviders), options.WithNoThinking())
	require.NoError(t, err)

	stream, err := provider.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "Hi"}}, nil)
	require.NoError(t, err)
	defer stream.Close()
	drainStream(t, stream)

	var req struct {
		ReasoningEffort string `json:"reasoning_effort"`
	}
	require.NoError(t, json.Unmarshal(body(), &req))
	assert.Equal(t, "none", req.ReasoningEffort)
}

func TestNamedCustomOpenAIProvider_GPT6NoneEffort(t *testing.T) {
	t.Parallel()

	for _, apiType := range []string{"openai_responses", "openai_chatcompletions"} {
		for _, model := range []string{"gpt-6-sol", "gpt-6-luna"} {
			for _, mode := range []string{"explicit", "no_thinking"} {
				t.Run(apiType+"/"+model+"/"+mode, func(t *testing.T) {
					t.Parallel()
					server, body := captureNamedCustomProviderRequestBody(t, apiType)
					cfg := &latest.ModelConfig{Provider: "my_openai", Model: model}
					opts := []options.Opt{options.WithProviders(map[string]latest.ProviderConfig{
						"my_openai": {BaseURL: server.URL, TokenKey: "MY_TOKEN", APIType: apiType},
					})}
					if mode == "explicit" {
						cfg.ThinkingBudget = &latest.ThinkingBudget{Effort: "none"}
					} else {
						opts = append(opts, options.WithNoThinking())
					}
					env := environment.NewMapEnvProvider(map[string]string{"MY_TOKEN": "secret"})
					p, err := fullTestRegistry().New(t.Context(), cfg, env, opts...)
					require.NoError(t, err)
					stream, err := p.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hi"}}, []tools.Tool{{
						Name: "get_time", Description: "get the time",
						Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
					}})
					require.NoError(t, err)
					defer stream.Close()
					drainStream(t, stream)
					var req struct {
						ReasoningEffort string `json:"reasoning_effort"`
						Reasoning       struct {
							Effort string `json:"effort"`
						} `json:"reasoning"`
					}
					require.NoError(t, json.Unmarshal(body(), &req))
					if apiType == "openai_responses" {
						assert.Equal(t, "none", req.Reasoning.Effort)
					} else {
						assert.Equal(t, "none", req.ReasoningEffort)
					}
				})
			}
		}
	}
}
