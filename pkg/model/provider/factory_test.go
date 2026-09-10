package provider

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/tools"
)

// fakeProvider is a Provider stub used to verify factory dispatch.
type fakeProvider struct {
	id modelsdev.ID
}

func (f *fakeProvider) ID() modelsdev.ID { return f.id }
func (f *fakeProvider) CreateChatCompletionStream(_ context.Context, _ []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeProvider) BaseConfig() base.Config { return base.Config{} }

func tagFactory(id string) providerFactory {
	return func(_ context.Context, _ *latest.ModelConfig, _ environment.Provider, _ ...options.Opt) (Provider, error) {
		return &fakeProvider{id: modelsdev.NewID("test", id)}, nil
	}
}

// TestCreateDirectProvider_DispatchByType verifies that resolveProviderType's
// output is mapped to the right factory entry for every supported value,
// including the OpenAI api_type aliases.
func TestCreateDirectProvider_DispatchByType(t *testing.T) {
	t.Parallel()
	r := NewRegistry(map[string]providerFactory{
		"openai":                 tagFactory("openai"),
		"openai_chatcompletions": tagFactory("openai_chatcompletions"),
		"openai_responses":       tagFactory("openai_responses"),
		"anthropic":              tagFactory("anthropic"),
		"google":                 tagFactory("google"),
		"dmr":                    tagFactory("dmr"),
		"amazon-bedrock":         tagFactory("amazon-bedrock"),
	})

	tests := []struct {
		name     string
		cfg      *latest.ModelConfig
		expectID string
	}{
		{
			name:     "openai",
			cfg:      &latest.ModelConfig{Provider: "openai", Model: "gpt-4o"},
			expectID: "openai",
		},
		{
			name:     "openai_chatcompletions via api_type override",
			cfg:      &latest.ModelConfig{Provider: "openai", Model: "gpt-4o", ProviderOpts: map[string]any{"api_type": "openai_chatcompletions"}},
			expectID: "openai_chatcompletions",
		},
		{
			name:     "openai_responses via api_type override",
			cfg:      &latest.ModelConfig{Provider: "openai", Model: "gpt-5", ProviderOpts: map[string]any{"api_type": "openai_responses"}},
			expectID: "openai_responses",
		},
		{
			name:     "anthropic",
			cfg:      &latest.ModelConfig{Provider: "anthropic", Model: "claude-sonnet-4-0"},
			expectID: "anthropic",
		},
		{
			name:     "google",
			cfg:      &latest.ModelConfig{Provider: "google", Model: "gemini-2.5-flash"},
			expectID: "google",
		},
		{
			name:     "dmr",
			cfg:      &latest.ModelConfig{Provider: "dmr", Model: "ai/llama3.2"},
			expectID: "dmr",
		},
		{
			name:     "amazon-bedrock",
			cfg:      &latest.ModelConfig{Provider: "amazon-bedrock", Model: "anthropic.claude-3-sonnet"},
			expectID: "amazon-bedrock",
		},
		{
			name:     "alias resolves to openai",
			cfg:      &latest.ModelConfig{Provider: "mistral", Model: "mistral-large-latest"},
			expectID: "openai",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p, err := r.createDirectProvider(t.Context(), tt.cfg, environment.NewNoEnvProvider())
			require.NoError(t, err)
			leaf := unwrapProvider(p)
			fp, ok := leaf.(*fakeProvider)
			require.True(t, ok, "expected fakeProvider, got %T", leaf)
			assert.Equal(t, tt.expectID, fp.id.Model)
		})
	}
}

// TestCreateDirectProvider_UnknownProviderType verifies the previously
// unreachable error branch when the resolved provider type is not registered.
func TestCreateDirectProvider_UnknownProviderType(t *testing.T) {
	t.Parallel()
	r := NewRegistry(map[string]providerFactory{
		"openai": tagFactory("openai"),
	})

	cfg := &latest.ModelConfig{Provider: "completely-unknown", Model: "x"}

	_, err := r.createDirectProvider(t.Context(), cfg, environment.NewNoEnvProvider())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown provider type")
	assert.Contains(t, err.Error(), "completely-unknown")
}

// TestCreateDirectProvider_FactoryError ensures errors returned by a factory
// are propagated unchanged to the caller.
func TestCreateDirectProvider_FactoryError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("boom")
	r := NewRegistry(map[string]providerFactory{
		"openai": func(_ context.Context, _ *latest.ModelConfig, _ environment.Provider, _ ...options.Opt) (Provider, error) {
			return nil, sentinel
		},
	})

	_, err := r.createDirectProvider(t.Context(), &latest.ModelConfig{Provider: "openai", Model: "gpt-4o"}, environment.NewNoEnvProvider())
	require.ErrorIs(t, err, sentinel)
}

// TestCreateDirectProvider_AppliesProviderDefaults verifies that the registry
// receives the *enhanced* config (i.e. applyProviderDefaults has run) before
// dispatch — the BaseURL from a custom provider must be visible to the factory.
func TestCreateDirectProvider_AppliesProviderDefaults(t *testing.T) {
	t.Parallel()
	var got *latest.ModelConfig
	r := NewRegistry(map[string]providerFactory{
		"openai_chatcompletions": func(_ context.Context, cfg *latest.ModelConfig, _ environment.Provider, _ ...options.Opt) (Provider, error) {
			got = cfg
			return &fakeProvider{id: modelsdev.NewID("test", "captured")}, nil
		},
	})

	customProviders := map[string]latest.ProviderConfig{
		"my_gateway": {
			APIType:  "openai_chatcompletions",
			BaseURL:  "https://api.gateway.example/v1",
			TokenKey: "GW_TOKEN",
		},
	}

	cfg := &latest.ModelConfig{Provider: "my_gateway", Model: "gpt-4o"}

	_, err := r.createDirectProvider(
		t.Context(), cfg, environment.NewNoEnvProvider(),
		options.WithProviders(customProviders),
	)
	require.NoError(t, err)

	require.NotNil(t, got)
	assert.Equal(t, "https://api.gateway.example/v1", got.BaseURL, "factory should receive enhanced BaseURL")
	assert.Equal(t, "GW_TOKEN", got.TokenKey)
	assert.Equal(t, "openai_chatcompletions", got.ProviderOpts["api_type"])
}

// TestCreateDirectProvider_BypassModelsGateway verifies that a model with
// bypass_models_gateway set clears the gateway option before the leaf factory
// runs, so the provider dials its endpoint directly. Models without the flag
// keep the gateway.
func TestCreateDirectProvider_BypassModelsGateway(t *testing.T) {
	t.Parallel()

	const gateway = "https://gateway.example.com"

	tests := []struct {
		name        string
		bypass      bool
		wantGateway string
	}{
		{name: "bypass clears gateway", bypass: true, wantGateway: ""},
		{name: "no bypass keeps gateway", bypass: false, wantGateway: gateway},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var gotOpts options.ModelOptions
			r := NewRegistry(map[string]providerFactory{
				"openai": func(_ context.Context, _ *latest.ModelConfig, _ environment.Provider, opts ...options.Opt) (Provider, error) {
					for _, opt := range opts {
						opt(&gotOpts)
					}
					return &fakeProvider{id: modelsdev.NewID("test", "captured")}, nil
				},
			})

			cfg := &latest.ModelConfig{Provider: "openai", Model: "gpt-4o", BypassModelsGateway: tt.bypass}

			_, err := r.createDirectProvider(
				t.Context(), cfg, environment.NewNoEnvProvider(),
				options.WithGateway(gateway),
			)
			require.NoError(t, err)
			assert.Equal(t, tt.wantGateway, gotOpts.Gateway())
		})
	}
}

// TestCreateDirectProvider_CustomBaseURLImpliesGatewayBypass verifies that a
// custom base_url — set directly on the model or inherited from a custom
// provider definition — implies bypass_models_gateway: the gateway option is
// cleared before the leaf factory runs. Built-in alias default base URLs and
// custom providers without a base_url keep the gateway.
func TestCreateDirectProvider_CustomBaseURLImpliesGatewayBypass(t *testing.T) {
	t.Parallel()

	const gateway = "https://gateway.example.com"

	customProviders := map[string]latest.ProviderConfig{
		"my_endpoint": {
			APIType:  "openai",
			BaseURL:  "https://llm.internal.example.com/v1",
			TokenKey: "MY_TOKEN",
		},
		"my_anthropic": {
			Provider: "anthropic",
			TokenKey: "MY_ANTHROPIC_KEY",
		},
	}

	tests := []struct {
		name        string
		cfg         *latest.ModelConfig
		wantGateway string
	}{
		{
			name:        "model base_url bypasses gateway",
			cfg:         &latest.ModelConfig{Provider: "openai", Model: "gpt-4o", BaseURL: "http://localhost:8000/v1"},
			wantGateway: "",
		},
		{
			name:        "custom provider base_url bypasses gateway",
			cfg:         &latest.ModelConfig{Provider: "my_endpoint", Model: "llama-3"},
			wantGateway: "",
		},
		{
			name:        "custom provider without base_url keeps gateway",
			cfg:         &latest.ModelConfig{Provider: "my_anthropic", Model: "claude-sonnet-4-5"},
			wantGateway: gateway,
		},
		{
			name:        "alias default base_url keeps gateway",
			cfg:         &latest.ModelConfig{Provider: "mistral", Model: "mistral-large-latest"},
			wantGateway: gateway,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var gotOpts options.ModelOptions
			leaf := func(_ context.Context, _ *latest.ModelConfig, _ environment.Provider, opts ...options.Opt) (Provider, error) {
				for _, opt := range opts {
					opt(&gotOpts)
				}
				return &fakeProvider{id: modelsdev.NewID("test", "captured")}, nil
			}
			r := NewRegistry(map[string]providerFactory{
				"openai":    leaf,
				"anthropic": leaf,
			})

			_, err := r.createDirectProvider(
				t.Context(), tt.cfg, environment.NewNoEnvProvider(),
				options.WithGateway(gateway),
				options.WithProviders(customProviders),
			)
			require.NoError(t, err)
			assert.Equal(t, tt.wantGateway, gotOpts.Gateway())
		})
	}
}

// TestNewWithModels_BypassModelsGatewayRouting verifies that a routing model
// with bypass_models_gateway propagates the bypass to its fallback and routed
// targets (the router itself makes no HTTP calls).
func TestNewWithModels_BypassModelsGatewayRouting(t *testing.T) {
	t.Parallel()

	const gateway = "https://gateway.example.com"

	tests := []struct {
		name        string
		bypass      bool
		wantGateway string
	}{
		{name: "router bypass clears gateway for children", bypass: true, wantGateway: ""},
		{name: "router without bypass keeps gateway for children", bypass: false, wantGateway: gateway},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var gateways []string
			r := NewRegistry(map[string]providerFactory{
				"openai": func(_ context.Context, _ *latest.ModelConfig, _ environment.Provider, opts ...options.Opt) (Provider, error) {
					var probe options.ModelOptions
					for _, opt := range opts {
						if opt != nil {
							opt(&probe)
						}
					}
					gateways = append(gateways, probe.Gateway())
					return &fakeProvider{id: modelsdev.NewID("openai", "captured")}, nil
				},
			})

			cfg := &latest.ModelConfig{
				Provider:            "openai",
				Model:               "gpt-4o",
				BypassModelsGateway: tt.bypass,
				Routing:             []latest.RoutingRule{{Model: "openai/gpt-4o-mini", Examples: []string{"hi"}}},
			}

			_, err := r.NewWithModels(
				t.Context(), cfg, nil, environment.NewNoEnvProvider(),
				options.WithGateway(gateway),
			)
			require.NoError(t, err)
			// One call for the fallback, one for the routed target.
			require.Len(t, gateways, 2)
			for _, g := range gateways {
				assert.Equal(t, tt.wantGateway, g)
			}
		})
	}
}

// TestCreateDirectProvider_ResolvesOpenAIVendorOption verifies that
// createDirectProvider computes the genuine-OpenAI-vendor bit from the fully
// resolved (post-applyProviderDefaults) config and passes it to the leaf
// factory via options.WithOpenAIVendor \u2014 never via ProviderOpts, which is
// public, user-controllable config. A spoofed provider_opts.openai_vendor
// key must have zero effect on the resolved value either way.
func TestCreateDirectProvider_ResolvesOpenAIVendorOption(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		cfg             *latest.ModelConfig
		customProviders map[string]latest.ProviderConfig
		want            bool
	}{
		{
			name: "direct openai provider",
			cfg:  &latest.ModelConfig{Provider: "openai", Model: "gpt-5.6"},
			want: true,
		},
		{
			name: "azure provider",
			cfg:  &latest.ModelConfig{Provider: "azure", Model: "gpt-5.6"},
			want: true,
		},
		{
			name: "vercel with explicit openai/-qualified model",
			cfg:  &latest.ModelConfig{Provider: "vercel", Model: "openai/gpt-5.6-sol"},
			want: true,
		},
		{
			name: "xai alias is never a genuine OpenAI vendor",
			cfg:  &latest.ModelConfig{Provider: "xai", Model: "gpt-5.6"},
			want: false,
		},
		{
			name: "mistral alias is never a genuine OpenAI vendor",
			cfg:  &latest.ModelConfig{Provider: "mistral", Model: "gpt-5.6-sol"},
			want: false,
		},
		{
			name: "named custom provider with omitted underlying provider resolves to OpenAI vendor",
			cfg:  &latest.ModelConfig{Provider: "my_openai", Model: "gpt-5.6"},
			customProviders: map[string]latest.ProviderConfig{
				"my_openai": {BaseURL: "https://api.openai.com/v1", TokenKey: "MY_KEY"},
			},
			want: true,
		},
		{
			name: "custom provider explicitly pointed at anthropic is not an OpenAI vendor",
			cfg:  &latest.ModelConfig{Provider: "claude_gateway", Model: "claude-x"},
			customProviders: map[string]latest.ProviderConfig{
				"claude_gateway": {Provider: "anthropic", BaseURL: "https://gateway.example.com"},
			},
			want: false,
		},
		{
			name: "spoofed provider_opts.openai_vendor=true on xai is ignored",
			cfg: &latest.ModelConfig{
				Provider:     "xai",
				Model:        "gpt-5.6",
				ProviderOpts: map[string]any{"openai_vendor": true},
			},
			want: false,
		},
		{
			name: "spoofed provider_opts.openai_vendor=false on a named custom OpenAI provider is ignored",
			cfg: &latest.ModelConfig{
				Provider:     "my_openai",
				Model:        "gpt-5.6",
				ProviderOpts: map[string]any{"openai_vendor": false},
			},
			customProviders: map[string]latest.ProviderConfig{
				"my_openai": {BaseURL: "https://api.openai.com/v1", TokenKey: "MY_KEY"},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var probe options.ModelOptions
			r := NewRegistry(map[string]providerFactory{
				"openai":                 tagFactoryCapturingOpts(&probe),
				"openai_chatcompletions": tagFactoryCapturingOpts(&probe),
				"openai_responses":       tagFactoryCapturingOpts(&probe),
				"anthropic":              tagFactoryCapturingOpts(&probe),
			})

			var opts []options.Opt
			if tt.customProviders != nil {
				opts = append(opts, options.WithProviders(tt.customProviders))
			}

			_, err := r.createDirectProvider(t.Context(), tt.cfg, environment.NewNoEnvProvider(), opts...)
			require.NoError(t, err)
			assert.Equal(t, tt.want, probe.OpenAIVendor())
		})
	}
}

// tagFactoryCapturingOpts returns a providerFactory that replays every
// received Opt onto into, so tests can inspect the resolved ModelOptions.
func tagFactoryCapturingOpts(into *options.ModelOptions) providerFactory {
	return func(_ context.Context, _ *latest.ModelConfig, _ environment.Provider, opts ...options.Opt) (Provider, error) {
		for _, opt := range opts {
			if opt != nil {
				opt(into)
			}
		}
		return &fakeProvider{id: modelsdev.NewID("test", "captured")}, nil
	}
}

func TestRegistry_OpenAIProtocolVariantsFallBackToOpenAI(t *testing.T) {
	t.Parallel()

	called := 0
	r := NewRegistry(map[string]Factory{
		"openai": func(context.Context, *latest.ModelConfig, environment.Provider, ...options.Opt) (Provider, error) {
			called++
			return nil, nil
		},
	})

	assert.True(t, r.Has("openai"))
	assert.True(t, r.Has("openai_chatcompletions"))
	assert.True(t, r.Has("openai_responses"))
	assert.False(t, r.Has("anthropic"))
	assert.Equal(t, []string{"openai"}, r.Types())

	// A custom OpenAI-compatible provider pinning an api_type resolves to a
	// protocol variant and must still be served by the "openai" factory.
	cfg := &latest.ModelConfig{Provider: "corp", Model: "m"}
	providers := map[string]latest.ProviderConfig{"corp": {BaseURL: "https://llm.corp.example", APIType: "openai_responses"}}
	assert.Equal(t, "openai_responses", ResolveType(cfg, providers))
	_, err := r.New(t.Context(), cfg, environment.NewEnvListProvider(nil), options.WithProviders(providers))
	require.NoError(t, err)
	assert.Equal(t, 1, called)
}
