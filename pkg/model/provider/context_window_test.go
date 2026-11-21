package provider

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

func TestConstructionContextWindowCatalogueIdentity(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, provider, model, publisher, inheritedPublisher, apiType, wantID string
		window                                                                int64
	}{
		{name: "environment Vertex identity", provider: "google", model: "${MODEL}", publisher: "anthropic", wantID: "anthropic/claude"},
		{name: "anthropic publisher", provider: "google", model: "claude", publisher: "anthropic", wantID: "anthropic/claude"},
		{name: "case folded publisher", provider: "google", model: "claude", publisher: "Anthropic", wantID: "anthropic/claude"},
		{name: "publisher whitespace matches runtime mapping", provider: "google", model: "claude", publisher: " anthropic ", wantID: "anthropic/claude"},
		{name: "meta publisher keeps prefix", provider: "google", model: "meta/llama", publisher: "meta", wantID: "google-vertex/meta/llama"},
		{name: "inherited publisher and underlying provider", provider: "custom", model: "${MODEL}", inheritedPublisher: "anthropic", wantID: "anthropic/claude"},
		{name: "model publisher overrides inherited publisher", provider: "custom", model: "meta/llama", publisher: "meta", inheritedPublisher: "anthropic", wantID: "google-vertex/meta/llama"},
		{name: "google publisher stays gemini", provider: "google", model: "gemini", publisher: "google", wantID: "google/gemini"},
		{name: "no publisher stays gemini", provider: "google", model: "gemini", wantID: "google/gemini"},
		{name: "api dispatch is not catalogue identity", provider: "custom", model: "claude", publisher: "anthropic", apiType: "openai_chatcompletions", wantID: "google/claude"},
		{name: "at cap", provider: "google", model: "claude", publisher: "anthropic", window: 128000},
		{name: "unknown catalogue model", provider: "google", model: "unknown", publisher: "anthropic"},
		{name: "catalogue model without limit", provider: "google", model: "no-limit", publisher: "anthropic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := &modelsdev.Database{Providers: map[string]modelsdev.Provider{
				"anthropic":     {Models: map[string]modelsdev.Model{"claude": {Limit: modelsdev.Limit{Context: 128000}}, "no-limit": {}}},
				"google-vertex": {Models: map[string]modelsdev.Model{"meta/llama": {Limit: modelsdev.Limit{Context: 128000}}}},
				"google":        {Models: map[string]modelsdev.Model{"gemini": {Limit: modelsdev.Limit{Context: 128000}}, "claude": {Limit: modelsdev.Limit{Context: 128000}}}},
			}}
			window := tc.window
			if window == 0 {
				window = 200000
			}
			cfg := &latest.ModelConfig{Provider: tc.provider, Model: tc.model, ContextWindow: &window, ProviderOpts: map[string]any{}}
			if tc.publisher != "" {
				cfg.ProviderOpts["publisher"] = tc.publisher
			}
			providers := map[string]latest.ProviderConfig{"custom": {Provider: "google", APIType: tc.apiType, ProviderOpts: map[string]any{"publisher": tc.inheritedPublisher}}}
			calls := 0
			factory := func(_ context.Context, got *latest.ModelConfig, _ environment.Provider, _ ...options.Opt) (Provider, error) {
				calls++
				assert.Equal(t, "google", got.Provider, "catalogue identity must not change API dispatch")
				return &fakeProvider{}, nil
			}
			registry := NewRegistry(map[string]Factory{"google": factory, "openai": factory})
			_, err := registry.New(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{"MODEL": "claude"}), options.WithProviders(providers), options.WithModelsDevStore(modelsdev.NewDatabaseStore(db)))
			if tc.wantID == "" {
				require.NoError(t, err)
				require.Equal(t, 1, calls)
			} else {
				require.ErrorContains(t, err, "exceeds the context window of "+tc.wantID)
				require.Zero(t, calls)
			}
			assert.Equal(t, tc.provider, cfg.Provider)
			assert.Equal(t, tc.model, cfg.Model)
			publisher, _ := cfg.ProviderOpts["publisher"].(string)
			assert.Equal(t, tc.publisher, publisher, "validation must not mutate provider options")
		})
	}
}

func TestRoutingContextWindowValidation(t *testing.T) {
	t.Parallel()
	for _, named := range []bool{false, true} {
		t.Run(map[bool]string{false: "inline inherited legacy option", true: "named over cap"}[named], func(t *testing.T) {
			t.Parallel()
			ref := "local/target"
			models := map[string]latest.ModelConfig{}
			window := int64(200000)
			if named {
				ref = "target"
				models[ref] = latest.ModelConfig{Provider: "openai", Model: "${MODEL}", ContextWindow: &window}
			}
			var got *latest.ModelConfig
			registry := NewRegistry(map[string]Factory{"dmr": captureFactory(&got), "openai": captureFactory(&got)})
			store := modelsdev.NewDatabaseStore(&modelsdev.Database{Providers: map[string]modelsdev.Provider{"openai": {Models: map[string]modelsdev.Model{"gpt-4o": {Limit: modelsdev.Limit{Context: 128000}}}}}})
			_, err := registry.NewWithModels(t.Context(), &latest.ModelConfig{Provider: "dmr", Model: "fallback", Routing: []latest.RoutingRule{{Model: ref, Examples: []string{"test"}}}}, models,
				environment.NewMapEnvProvider(map[string]string{"MODEL": "gpt-4o"}),
				options.WithProviders(map[string]latest.ProviderConfig{"local": {Provider: "openai", ProviderOpts: map[string]any{"context_size": 8192}}}), options.WithModelsDevStore(store))
			if named {
				require.ErrorContains(t, err, "exceeds the context window of openai/gpt-4o")
			} else {
				require.ErrorContains(t, err, "context_size is only used by the dmr provider")
			}
			require.NotNil(t, got, "the valid routing fallback is built first")
			assert.Equal(t, "dmr", got.Provider, "invalid routing target must not reach its factory")
		})
	}
}

func TestConstructionRejectsInvalidContextWindowDeclarations(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, provider, wantErr string
		window                  int64
		contextSize             int
	}{
		{name: "zero", provider: "openai", wantErr: "context_window must be greater than 0"},
		{name: "negative", provider: "dmr", window: -1, wantErr: "context_window must be greater than 0"},
		{name: "DMR conflict", provider: "dmr", window: 8192, contextSize: 4096, wantErr: "context_window 8192 conflicts"},
		{name: "non-DMR legacy option", provider: "openai", window: 8192, contextSize: 4096, wantErr: "context_size is only used by the dmr provider"},
	} {
		for _, routed := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "/leaf", true: "/router"}[routed], func(t *testing.T) {
				t.Parallel()
				cfg := &latest.ModelConfig{Provider: tc.provider, Model: "model", ContextWindow: &tc.window}
				if tc.contextSize != 0 {
					cfg.ProviderOpts = map[string]any{"context_size": tc.contextSize}
				}
				if routed {
					cfg.Routing = []latest.RoutingRule{{Model: "openai/target", Examples: []string{"test"}}}
				}
				registry := NewRegistry(map[string]Factory{"openai": func(context.Context, *latest.ModelConfig, environment.Provider, ...options.Opt) (Provider, error) {
					t.Fatal("invalid declaration reached factory")
					return nil, nil
				}})
				_, err := registry.New(t.Context(), cfg, environment.NewNoEnvProvider())
				require.ErrorContains(t, err, tc.wantErr)
			})
		}
	}
}

// RAG and SDK callers can construct providers without a models.dev store;
// config preflight remains responsible for requiring the catalogue at load.
func TestRegistryContextWindowWithoutCatalogue(t *testing.T) {
	t.Parallel()
	for _, routed := range []bool{false, true} {
		t.Run(map[bool]string{false: "leaf", true: "router"}[routed], func(t *testing.T) {
			t.Parallel()
			window := int64(8192)
			cfg := &latest.ModelConfig{Provider: "openai", Model: "custom", ContextWindow: &window}
			if routed {
				cfg.Routing = []latest.RoutingRule{{Model: "openai/target", Examples: []string{"test"}}}
			}
			var got *latest.ModelConfig
			registry := NewRegistry(map[string]Factory{"openai": func(_ context.Context, cfg *latest.ModelConfig, _ environment.Provider, _ ...options.Opt) (Provider, error) {
				got = cfg
				return &fakeProvider{config: base.Config{ModelConfig: *cfg}}, nil
			}})
			p, err := registry.New(t.Context(), cfg, environment.NewNoEnvProvider())
			require.NoError(t, err)
			require.NotNil(t, p)
			require.NotNil(t, got)
			assert.Equal(t, &window, p.BaseConfig().ModelConfig.ContextWindow)
		})
	}
}

func TestConstructionContextWindowEnvironment(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, model, display, endpoint, provider, wantErr string
		window                                            int64
	}{
		{name: "bare shell identity", model: "$MODEL", window: 200000, wantErr: "exceeds the context window of openai/gpt-4o"},
		{name: "shell identity", model: "${MODEL}", window: 200000, wantErr: "exceeds the context window of openai/gpt-4o"},
		{name: "template identity", model: "${env.MODEL}", window: 200000, wantErr: "exceeds the context window of openai/gpt-4o"},
		{name: "display identity", model: "pinned-id", display: "${MODEL}", window: 200000, wantErr: "exceeds the context window of openai/gpt-4o"},
		{name: "inherited provider identity", provider: "proxy", model: "${MODEL}", window: 200000, wantErr: "exceeds the context window of openai/gpt-4o"},
		{name: "at cap", model: "${MODEL}", window: 128000},
		{name: "unknown identity", model: "${UNKNOWN}", window: 200000},
		{name: "missing identity", model: "${MISSING}", window: 8192, wantErr: `environment variable "MISSING" not set`},
		{name: "missing display identity", model: "gpt-4o", display: "${MISSING}", window: 8192, wantErr: `environment variable "MISSING" not set`},
		{name: "missing endpoint", model: "gpt-4o", endpoint: "${REMOTE_URL}", window: 8192, wantErr: `environment variable "REMOTE_URL" not set`},
		{name: "missing inherited endpoint", provider: "proxy", model: "gpt-4o", window: 8192, wantErr: `environment variable "REMOTE_URL" not set`},
		{name: "static cap before endpoint", model: "gpt-4o", endpoint: "${REMOTE_URL}", window: 200000, wantErr: "exceeds the context window of openai/gpt-4o"},
		{name: "missing runtime identity despite static display", model: "${MISSING}", display: "gpt-4o", window: 8192, wantErr: `environment variable "MISSING" not set`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			providerName := tc.provider
			if providerName == "" {
				providerName = "openai"
			}
			cfg := &latest.ModelConfig{Provider: providerName, Model: tc.model, DisplayModel: tc.display, BaseURL: tc.endpoint, ContextWindow: &tc.window}
			var got *latest.ModelConfig
			registry := NewRegistry(map[string]Factory{"openai": captureFactory(&got)})
			store := modelsdev.NewDatabaseStore(&modelsdev.Database{Providers: map[string]modelsdev.Provider{
				"openai": {Models: map[string]modelsdev.Model{"gpt-4o": {Limit: modelsdev.Limit{Context: 128000}}}},
			}})
			_, err := registry.New(t.Context(), cfg, environment.NewMapEnvProvider(map[string]string{"MODEL": "gpt-4o", "UNKNOWN": "custom"}),
				options.WithProviders(map[string]latest.ProviderConfig{"proxy": {Provider: "openai", BaseURL: "${REMOTE_URL}"}}), options.WithModelsDevStore(store))
			if tc.wantErr == "" {
				require.NoError(t, err)
				require.NotNil(t, got)
			} else {
				require.ErrorContains(t, err, tc.wantErr)
				require.Nil(t, got, "invalid used models must not reach their factory")
			}
			assert.Equal(t, tc.model, cfg.Model)
			assert.Equal(t, tc.endpoint, cfg.BaseURL)
		})
	}
}
