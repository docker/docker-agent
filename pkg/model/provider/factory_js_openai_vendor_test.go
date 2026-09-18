//go:build js && wasm

package provider_test

import (
	"context"
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
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/openai"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

// Exercise the shared factory under js/wasm using an explicit OpenAI registry.
func wasmTestRegistry() *provider.Registry {
	return provider.NewRegistry(map[string]provider.Factory{
		"openai": func(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (provider.Provider, error) {
			return openai.NewClient(ctx, cfg, env, opts...)
		},
	})
}

// captureNamedCustomProviderRequestBody starts a mock chat-completions server
// that records the last request body it received and replies with a minimal
// gpt-5.6 SSE stream.
func captureNamedCustomProviderRequestBody(t *testing.T) (server *httptest.Server, body func() []byte) {
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
		writeSSEChunk(w, map[string]any{
			"id": "test", "object": "chat.completion.chunk", "model": "gpt-5.6",
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": "Hello"}, "finish_reason": "stop"}},
		})
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}))
	t.Cleanup(server.Close)
	return server, func() []byte {
		mu.Lock()
		defer mu.Unlock()
		return received
	}
}

func writeSSEChunk(w http.ResponseWriter, data map[string]any) {
	jsonData, _ := json.Marshal(data)
	_, _ = w.Write([]byte("data: " + string(jsonData) + "\n\n"))
}

func drainStream(t *testing.T, stream chat.MessageStream) {
	t.Helper()
	for {
		if _, err := stream.Recv(); err != nil {
			return
		}
	}
}

// reasoningEffortOf decodes the reasoning_effort field of a captured
// chat-completions request body.
func reasoningEffortOf(t *testing.T, body []byte) string {
	t.Helper()
	var req struct {
		ReasoningEffort string `json:"reasoning_effort"`
	}
	require.NoError(t, json.Unmarshal(body, &req))
	return req.ReasoningEffort
}

// TestJSFactory_NamedCustomOpenAIProvider_NoThinking_SendsNoneEffort proves
// the js/wasm registry resolves a named custom OpenAI provider (providers:
// section, no explicit `provider:` override) to the genuine OpenAI vendor and
// threads gpt-5.6's real "none" reasoning effort through NoThinking(), not
// the generic "low" fallback used for OpenAI-compatible aliases fronting a
// different vendor.
func TestJSFactory_NamedCustomOpenAIProvider_NoThinking_SendsNoneEffort(t *testing.T) {
	t.Parallel()

	server, body := captureNamedCustomProviderRequestBody(t)

	customProviders := map[string]latest.ProviderConfig{
		"my_openai": {
			BaseURL:  server.URL,
			TokenKey: "MY_OPENAI_TOKEN",
			APIType:  "openai_chatcompletions",
		},
	}
	modelCfg := &latest.ModelConfig{Provider: "my_openai", Model: "gpt-5.6"}
	env := environment.NewMapEnvProvider(map[string]string{"MY_OPENAI_TOKEN": "secret"})

	provider, err := wasmTestRegistry().New(t.Context(), modelCfg, env,
		options.WithProviders(customProviders), options.WithNoThinking())
	require.NoError(t, err)

	stream, err := provider.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "Hi"}}, nil)
	require.NoError(t, err)
	defer stream.Close()
	drainStream(t, stream)

	assert.Equal(t, "none", reasoningEffortOf(t, body()))
}

// TestJSFactory_UnrecognizedAlias_KeepsLowFallback is the negative
// counterpart: xai/mistral merely speak the OpenAI wire protocol for a
// different vendor, so gpt-5.6 must keep the generic "low" fallback.
func TestJSFactory_UnrecognizedAlias_KeepsLowFallback(t *testing.T) {
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

			server, body := captureNamedCustomProviderRequestBody(t)

			modelCfg := &latest.ModelConfig{
				Provider: tt.provider,
				Model:    "gpt-5.6",
				BaseURL:  server.URL,
				ProviderOpts: map[string]any{
					"api_type": "openai_chatcompletions",
				},
			}
			env := environment.NewMapEnvProvider(map[string]string{tt.tokenKey: "secret"})

			p, err := wasmTestRegistry().New(t.Context(), modelCfg, env, options.WithNoThinking())
			require.NoError(t, err)

			stream, err := p.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "Hi"}}, nil)
			require.NoError(t, err)
			defer stream.Close()
			drainStream(t, stream)

			assert.Equal(t, "low", reasoningEffortOf(t, body()))
		})
	}
}

// TestJSFactory_SpoofedProviderOptsCannotOverrideResolution proves that
// provider_opts.openai_vendor (public, user-controllable config) has zero
// effect in either direction under js/wasm, matching the native adversarial
// coverage in named_custom_openai_provider_test.go.
func TestJSFactory_SpoofedProviderOptsCannotOverrideResolution(t *testing.T) {
	t.Parallel()

	t.Run("spoofed true on xai cannot force none", func(t *testing.T) {
		t.Parallel()
		server, body := captureNamedCustomProviderRequestBody(t)
		modelCfg := &latest.ModelConfig{
			Provider: "xai",
			Model:    "gpt-5.6",
			BaseURL:  server.URL,
			ProviderOpts: map[string]any{
				"api_type":      "openai_chatcompletions",
				"openai_vendor": true, // spoofed; must be ignored
			},
		}
		env := environment.NewMapEnvProvider(map[string]string{"XAI_API_KEY": "secret"})

		p, err := wasmTestRegistry().New(t.Context(), modelCfg, env, options.WithNoThinking())
		require.NoError(t, err)
		stream, err := p.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "Hi"}}, nil)
		require.NoError(t, err)
		defer stream.Close()
		drainStream(t, stream)
		assert.Equal(t, "low", reasoningEffortOf(t, body()))
	})

	t.Run("spoofed false on named custom OpenAI provider cannot suppress none", func(t *testing.T) {
		t.Parallel()
		server, body := captureNamedCustomProviderRequestBody(t)
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

		provider, err := wasmTestRegistry().New(t.Context(), modelCfg, env,
			options.WithProviders(customProviders), options.WithNoThinking())
		require.NoError(t, err)
		stream, err := provider.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "Hi"}}, nil)
		require.NoError(t, err)
		defer stream.Close()
		drainStream(t, stream)
		assert.Equal(t, "none", reasoningEffortOf(t, body()))
	})
}

// TestJSFactory_CallerSuppliedStaleOpenAIVendorIsOverridden proves that a
// stale options.WithOpenAIVendor value supplied by the caller (e.g. carried
// forward from a previous resolution) is always replaced by the factory's own
// resolution: createDirectProvider appends its computed Opt last, so it wins
// over anything the caller passed in, in either direction.
func TestJSFactory_CallerSuppliedStaleOpenAIVendorIsOverridden(t *testing.T) {
	t.Parallel()

	t.Run("stale true on a non-OpenAI alias is corrected to false", func(t *testing.T) {
		t.Parallel()
		server, body := captureNamedCustomProviderRequestBody(t)
		modelCfg := &latest.ModelConfig{
			Provider: "xai",
			Model:    "gpt-5.6",
			BaseURL:  server.URL,
			ProviderOpts: map[string]any{
				"api_type": "openai_chatcompletions",
			},
		}
		env := environment.NewMapEnvProvider(map[string]string{"XAI_API_KEY": "secret"})

		p, err := wasmTestRegistry().New(t.Context(), modelCfg, env,
			options.WithOpenAIVendor(true), options.WithNoThinking())
		require.NoError(t, err)
		stream, err := p.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "Hi"}}, nil)
		require.NoError(t, err)
		defer stream.Close()
		drainStream(t, stream)
		assert.Equal(t, "low", reasoningEffortOf(t, body()), "the factory's resolved value must override a stale caller-supplied true")
	})

	t.Run("stale false on a named custom OpenAI provider is corrected to true", func(t *testing.T) {
		t.Parallel()
		server, body := captureNamedCustomProviderRequestBody(t)
		customProviders := map[string]latest.ProviderConfig{
			"my_openai": {
				BaseURL:  server.URL,
				TokenKey: "MY_OPENAI_TOKEN",
				APIType:  "openai_chatcompletions",
			},
		}
		modelCfg := &latest.ModelConfig{Provider: "my_openai", Model: "gpt-5.6"}
		env := environment.NewMapEnvProvider(map[string]string{"MY_OPENAI_TOKEN": "secret"})

		provider, err := wasmTestRegistry().New(t.Context(), modelCfg, env,
			options.WithProviders(customProviders), options.WithOpenAIVendor(false), options.WithNoThinking())
		require.NoError(t, err)
		stream, err := provider.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "Hi"}}, nil)
		require.NoError(t, err)
		defer stream.Close()
		drainStream(t, stream)
		assert.Equal(t, "none", reasoningEffortOf(t, body()), "the factory's resolved value must override a stale caller-supplied false")
	})
}

// TestJSFactory_CloneWithOptions_PreservesOpenAIVendorBit is the js/wasm
// counterpart of clone_test.go's OpenAIVendor coverage: CloneWithOptions
// (used for e.g. title-generation and sampling clones) re-enters
// createDirectProvider, which must recompute isOpenAIVendor from the base
// config rather than trusting a stale copy, so the clone's NoThinking() path
// keeps sending gpt-5.6's real "none" effort too.
func TestJSFactory_CloneWithOptions_PreservesOpenAIVendorBit(t *testing.T) {
	t.Parallel()

	server, body := captureNamedCustomProviderRequestBody(t)

	customProviders := map[string]latest.ProviderConfig{
		"my_openai": {
			BaseURL:  server.URL,
			TokenKey: "MY_OPENAI_TOKEN",
			APIType:  "openai_chatcompletions",
		},
	}
	modelCfg := &latest.ModelConfig{Provider: "my_openai", Model: "gpt-5.6"}
	env := environment.NewMapEnvProvider(map[string]string{"MY_OPENAI_TOKEN": "secret"})

	baseProvider, err := wasmTestRegistry().New(t.Context(), modelCfg, env, options.WithProviders(customProviders))
	require.NoError(t, err)
	baseOpts := baseProvider.BaseConfig().ModelOptions
	require.True(t, baseOpts.OpenAIVendor(), "base provider should resolve the OpenAI vendor bit")

	cloned := provider.CloneWithOptions(t.Context(), baseProvider, options.WithNoThinking())
	clonedOpts := cloned.BaseConfig().ModelOptions
	require.True(t, clonedOpts.OpenAIVendor(), "clone must preserve the OpenAI vendor bit")

	stream, err := cloned.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "Hi"}}, nil)
	require.NoError(t, err)
	defer stream.Close()
	drainStream(t, stream)
	assert.Equal(t, "none", reasoningEffortOf(t, body()), "cloned NoThinking() path must still send gpt-5.6's real none effort")
}

// TestJSFactory_CloneWithOptions_KeepsOpenAIVendorFalse is the negative
// counterpart: cloning a provider built on a known non-OpenAI alias must not
// gain the OpenAI vendor bit.
func TestJSFactory_CloneWithOptions_KeepsOpenAIVendorFalse(t *testing.T) {
	t.Parallel()

	server, body := captureNamedCustomProviderRequestBody(t)

	modelCfg := &latest.ModelConfig{
		Provider: "xai",
		Model:    "gpt-5.6",
		BaseURL:  server.URL,
		ProviderOpts: map[string]any{
			"api_type": "openai_chatcompletions",
		},
	}
	env := environment.NewMapEnvProvider(map[string]string{"XAI_API_KEY": "secret"})

	baseProvider, err := wasmTestRegistry().New(t.Context(), modelCfg, env)
	require.NoError(t, err)
	baseOpts := baseProvider.BaseConfig().ModelOptions
	require.False(t, baseOpts.OpenAIVendor())

	cloned := provider.CloneWithOptions(t.Context(), baseProvider, options.WithNoThinking())
	clonedOpts := cloned.BaseConfig().ModelOptions
	require.False(t, clonedOpts.OpenAIVendor())

	stream, err := cloned.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "Hi"}}, nil)
	require.NoError(t, err)
	defer stream.Close()
	drainStream(t, stream)
	assert.Equal(t, "low", reasoningEffortOf(t, body()))
}
