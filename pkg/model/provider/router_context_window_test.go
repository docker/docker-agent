package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

func TestRouterContextWindowClampsOnlyFallback(t *testing.T) {
	t.Parallel()

	for _, namedFallback := range []bool{false, true} {
		t.Run(map[bool]string{false: "inline fallback", true: "named fallback"}[namedFallback], func(t *testing.T) {
			t.Parallel()
			type request struct {
				Model     string `json:"model"`
				MaxTokens int64  `json:"max_tokens"`
			}
			requests := make(chan request, 6)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body request
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				requests <- body
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
			}))
			defer server.Close()

			cfg := latest.ModelConfig{
				Provider: "custom", Model: "fallback", ContextWindow: new(int64(8192)),
				Routing: []latest.RoutingRule{
					{Model: "target", Examples: []string{"poetry"}},
					{Model: "custom/fallback", Examples: []string{"same endpoint"}},
				},
			}
			models := map[string]latest.ModelConfig{
				"target": {Provider: "custom", Model: "target", ContextWindow: new(int64(16384))},
			}
			if namedFallback {
				models["custom/fallback"] = latest.ModelConfig{Provider: "custom", Model: "fallback", ContextWindow: new(int64(32768))}
			}
			p, err := fullTestRegistry().NewWithModels(t.Context(), &cfg, models, environment.NewNoEnvProvider(), options.WithProviders(map[string]latest.ProviderConfig{
				"custom": {APIType: "openai_chatcompletions", BaseURL: server.URL, MaxTokens: new(int64(65536))},
			}))
			require.NoError(t, err)
			cloned, err := Clone(t.Context(), p)
			require.NoError(t, err)
			fallbackMax := int64(8192 - 1024)
			sameEndpointMax := int64(65536)
			if namedFallback {
				fallbackMax = 32768 - 1024
				sameEndpointMax = fallbackMax
			}
			for _, current := range []Provider{p, cloned} {
				for _, tc := range []struct {
					prompt, model string
					maxTokens     int64
				}{
					{prompt: "unrelated", model: "fallback", maxTokens: fallbackMax},
					{prompt: "poetry", model: "target", maxTokens: 16384 - 1024},
					{prompt: "same endpoint", model: "fallback", maxTokens: sameEndpointMax},
				} {
					stream, err := current.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: tc.prompt}}, nil)
					require.NoError(t, err)
					for {
						if _, err := stream.Recv(); err != nil {
							break
						}
					}
					stream.Close()
					got := <-requests
					assert.Equal(t, tc.model, got.Model)
					assert.Equal(t, tc.maxTokens, got.MaxTokens)
				}
			}
			assert.Equal(t, int64(8192), *cfg.ContextWindow)
			assert.Equal(t, int64(16384), *models["target"].ContextWindow)
			if namedFallback {
				assert.Equal(t, int64(32768), *models["custom/fallback"].ContextWindow)
			}
		})
	}
}

func TestRouterFallbackContextWindowDefaults(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		window    *int64
		modelOpts map[string]any
		display   string
	}{
		{name: "no declaration"},
		{name: "DMR engine override", window: new(int64(8192)), modelOpts: map[string]any{"context_size": 8192}},
		{name: "catalogue display identity", window: new(int64(8192)), modelOpts: map[string]any{"context_size": 8192}, display: "catalogue-name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var fallback *latest.ModelConfig
			registry := NewRegistry(map[string]Factory{"dmr": func(_ context.Context, cfg *latest.ModelConfig, _ environment.Provider, _ ...options.Opt) (Provider, error) {
				if cfg.Model == "runtime-name" {
					fallback = cfg
				}
				return &fakeProvider{}, nil
			}})
			cfg := latest.ModelConfig{
				Provider: "dmr", Model: "runtime-name", DisplayModel: tc.display, ContextWindow: tc.window, ProviderOpts: tc.modelOpts,
				Routing: []latest.RoutingRule{{Model: "dmr/target", Examples: []string{"poetry"}}},
			}
			store := modelsdev.NewDatabaseStore(&modelsdev.Database{Providers: map[string]modelsdev.Provider{"dmr": {Models: map[string]modelsdev.Model{
				"runtime-name":   {Limit: modelsdev.Limit{Context: 4096}},
				"catalogue-name": {Limit: modelsdev.Limit{Context: 8192}},
			}}}})
			// The catalogue-identity case must check the display model at both boundaries.
			if tc.display == "" {
				store = modelsdev.NewDatabaseStore(&modelsdev.Database{})
			}
			_, err := registry.New(t.Context(), &cfg, environment.NewNoEnvProvider(),
				options.WithProviders(map[string]latest.ProviderConfig{"dmr": {Provider: "dmr", ProviderOpts: map[string]any{"context_size": 4096}}}),
				options.WithModelsDevStore(store))
			require.NoError(t, err)
			require.NotNil(t, fallback)
			assert.Equal(t, tc.window, fallback.ContextWindow)
			if tc.modelOpts != nil {
				assert.Equal(t, int64(8192), latest.ContextSizeFromProviderOpts(fallback.ProviderOpts))
			} else {
				assert.Equal(t, int64(4096), latest.ContextSizeFromProviderOpts(fallback.ProviderOpts))
			}
			if tc.window != nil {
				assert.Equal(t, tc.display, fallback.DisplayModel)
			}
		})
	}
}
