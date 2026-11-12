package teamloader

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/config/legacy"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/openai"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

func TestLoadValidatesEffectiveContextWindows(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, version, modelOpts string
		provider                 latest.ProviderConfig
		wantErr                  string
	}{
		{
			name:      "global dmr alias",
			version:   "16",
			provider:  latest.ProviderConfig{Provider: "dmr"},
			modelOpts: "    provider_opts:\n      context_size: 8192\n",
		},
		{
			name:      "migrated non-dmr",
			version:   "15",
			provider:  latest.ProviderConfig{Provider: "openai", BaseURL: "https://provider.example.test/v1"},
			modelOpts: "    provider_opts:\n      context_size: 8192\n",
			wantErr:   "context_size is only used by the dmr provider",
		},
		{
			name:      "inherited dmr conflict",
			version:   "16",
			provider:  latest.ProviderConfig{Provider: "dmr", ProviderOpts: map[string]any{"context_size": 8192}},
			modelOpts: "    context_window: 4096\n",
			wantErr:   "context_window 4096 conflicts with providers.local.provider_opts.context_size 8192",
		},
		{
			name:     "inherited non-dmr",
			version:  "16",
			provider: latest.ProviderConfig{Provider: "openai", BaseURL: "https://provider.example.test/v1", ProviderOpts: map[string]any{"context_size": 8192}},
			wantErr:  "context_size is only used by the dmr provider",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			data := []byte("version: \"" + tc.version + "\"\nmodels:\n  m:\n    provider: local\n    model: custom\n" + tc.modelOpts + "agents:\n  root:\n    model: m\n    instruction: test\n")
			constructed := errors.New("provider construction reached")
			registry := provider.NewRegistry(map[string]provider.Factory{
				"dmr": func(context.Context, *latest.ModelConfig, environment.Provider, ...options.Opt) (provider.Provider, error) {
					return nil, constructed
				},
			})
			runConfig := &config.RuntimeConfig{
				Config:                 config.Config{Providers: map[string]latest.ProviderConfig{"local": tc.provider}},
				ModelsDevStoreOverride: modelsdev.NewDatabaseStore(&modelsdev.Database{}),
				EnvProviderForTests:    environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "test"}),
			}
			_, err := LoadWithConfig(t.Context(), config.NewBytesSource("context-window.yaml", data), runConfig, withTestProviderRegistry(WithProviderRegistry(registry), WithConfigVersions(legacy.Register))...)
			if tc.wantErr == "" {
				require.ErrorIs(t, err, constructed)
			} else {
				require.ErrorContains(t, err, tc.wantErr)
			}
		})
	}
}

func TestLoadValidatesInlineCompactionContextWindows(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, provider, modelExtra, agentExtra, providerCompaction, wantErr string
	}{
		{
			name:       "agent inline non-dmr",
			provider:   "openai",
			agentExtra: "    compaction_model: local/summary\n",
			wantErr:    `agents.root.compaction_model: providers.local.provider_opts.context_size is only used by the dmr provider`,
		},
		{
			name:       "model inline non-dmr",
			provider:   "openai",
			modelExtra: "    compaction_model: local/summary\n",
			wantErr:    `agents.root.compaction_model: providers.local.provider_opts.context_size is only used by the dmr provider`,
		},
		{
			name:               "provider inline non-dmr",
			provider:           "openai",
			providerCompaction: "local/summary",
			wantErr:            `agents.root.compaction_model: providers.local.provider_opts.context_size is only used by the dmr provider`,
		},
		{
			name:       "inline dmr is valid",
			provider:   "dmr",
			agentExtra: "    compaction_model: local/summary\n",
		},
		{
			name:       "unused provider is not validated",
			provider:   "openai",
			agentExtra: "    compaction_model: dmr/summary\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			data := []byte("version: \"16\"\nmodels:\n  m:\n    provider: primary\n    model: custom\n" + tc.modelExtra + "agents:\n  root:\n    model: m\n    instruction: test\n" + tc.agentExtra)
			constructed := errors.New("provider construction reached")
			registry := provider.NewRegistry(map[string]provider.Factory{
				"dmr": func(context.Context, *latest.ModelConfig, environment.Provider, ...options.Opt) (provider.Provider, error) {
					return nil, constructed
				},
			})
			runConfig := &config.RuntimeConfig{
				Config: config.Config{Providers: map[string]latest.ProviderConfig{
					"primary": {Provider: "dmr", CompactionModel: tc.providerCompaction},
					"local":   {Provider: tc.provider, BaseURL: "https://provider.example.test/v1", ProviderOpts: map[string]any{"context_size": 8192}},
				}},
				ModelsDevStoreOverride: modelsdev.NewDatabaseStore(&modelsdev.Database{}),
				EnvProviderOverride:    environment.NewNoEnvProvider(),
			}
			_, err := LoadWithConfig(t.Context(), config.NewBytesSource("inline-context-window.yaml", data), runConfig, withTestProviderRegistry(WithProviderRegistry(registry))...)
			if tc.wantErr == "" {
				require.ErrorIs(t, err, constructed)
			} else {
				require.ErrorContains(t, err, tc.wantErr)
				require.NotErrorIs(t, err, constructed, "invalid inline defaults must fail before any provider is built")
			}
		})
	}
}

func TestLoadValidatesEnvironmentModelContextWindowCap(t *testing.T) {
	t.Parallel()

	data := []byte("version: \"16\"\nmodels:\n  m:\n    provider: openai\n    model: ${env.MODEL}\n    context_window: 200000\nagents:\n  root:\n    model: m\n    instruction: test\n")
	constructed := errors.New("provider construction reached")
	registry := provider.NewRegistry(map[string]provider.Factory{
		"openai": func(context.Context, *latest.ModelConfig, environment.Provider, ...options.Opt) (provider.Provider, error) {
			return nil, constructed
		},
	})
	runConfig := &config.RuntimeConfig{
		ModelsDevStoreOverride: modelsdev.NewDatabaseStore(&modelsdev.Database{Providers: map[string]modelsdev.Provider{
			"openai": {Models: map[string]modelsdev.Model{"gpt-4o": {Limit: modelsdev.Limit{Context: 128000}}}},
		}}),
		EnvProviderOverride: environment.NewMapEnvProvider(map[string]string{"MODEL": "gpt-4o", "OPENAI_API_KEY": "test"}),
	}
	_, err := LoadWithConfig(t.Context(), config.NewBytesSource("environment-context-window.yaml", data), runConfig, withTestProviderRegistry(WithProviderRegistry(registry))...)
	require.ErrorContains(t, err, "context_window 200000 exceeds the context window of openai/gpt-4o, which is 128000 tokens")
	require.NotErrorIs(t, err, constructed)
}

// Exercise sibling construction paths without the config preflight: inline
// declarations must be checked at the common provider boundary as well.
func TestAuxiliaryModelContextWindowValidation(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"fallback", "title", "compaction"} {
		for _, declaration := range []string{"inline legacy default", "named over cap", "unused auxiliary is not traversed"} {
			t.Run(kind+"/"+declaration, func(t *testing.T) {
				t.Parallel()
				window := int64(200000)
				ref := "local/gpt-4o"
				cfg := &latest.Config{
					Models:    map[string]latest.ModelConfig{"primary": {Provider: "dmr", Model: "main"}},
					Providers: map[string]latest.ProviderConfig{"local": {Provider: "openai", ProviderOpts: map[string]any{"context_size": 8192}}},
				}
				if declaration != "inline legacy default" {
					ref = "aux"
					cfg.Models[ref] = latest.ModelConfig{Provider: "openai", Model: "${MODEL}", ContextWindow: &window}
				}
				// References on an auxiliary model are not recursively constructed.
				if declaration == "unused auxiliary is not traversed" {
					ref = "aux"
					cfg.Models["aux"] = latest.ModelConfig{Provider: "dmr", Model: "unused", TitleModel: "local/invalid", CompactionModel: "local/invalid"}
				}
				primary := cfg.Models["primary"]
				agent := &latest.AgentConfig{Name: "root", Model: "primary"}
				switch kind {
				case "fallback":
					agent.Fallback = &latest.FallbackConfig{Models: []string{ref}}
				case "title":
					primary.TitleModel = ref
				case "compaction":
					agent.CompactionModel = ref
				}
				cfg.Models["primary"] = primary
				calls := 0
				factory := func(context.Context, *latest.ModelConfig, environment.Provider, ...options.Opt) (provider.Provider, error) {
					calls++
					return nil, nil
				}
				registry := provider.NewRegistry(map[string]provider.Factory{"openai": factory, "dmr": factory})
				runConfig := &config.RuntimeConfig{
					ModelsDevStoreOverride: modelsdev.NewDatabaseStore(&modelsdev.Database{Providers: map[string]modelsdev.Provider{
						"openai": {Models: map[string]modelsdev.Model{"gpt-4o": {Limit: modelsdev.Limit{Context: 128000}}}},
					}}),
					EnvProviderOverride: environment.NewMapEnvProvider(map[string]string{"MODEL": "gpt-4o", "OPENAI_API_KEY": "test"}),
				}
				var err error
				switch kind {
				case "fallback":
					_, err = getFallbackModelsForAgent(t.Context(), cfg, agent, runConfig, registry, nil)
				case "title":
					_, err = getTitleModelForAgent(t.Context(), cfg, agent, runConfig, registry, nil)
				case "compaction":
					_, err = getCompactionModelForAgent(t.Context(), cfg, agent, runConfig, registry, nil)
				}
				switch declaration {
				case "inline legacy default":
					require.ErrorContains(t, err, "context_size is only used by the dmr provider")
				case "named over cap":
					require.ErrorContains(t, err, "exceeds the context window of openai/gpt-4o")
				default:
					require.NoError(t, err)
					require.Equal(t, 1, calls)
					return
				}
				require.Zero(t, calls, "invalid declarations must fail before the leaf factory")
			})
		}
	}
}

func TestLoadContextWindowOptionalCandidates(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, remote string
	}{
		{name: "model endpoint", remote: "    provider: openai\n    model: custom\n    base_url: ${REMOTE_URL}\n"},
		{name: "inherited endpoint", remote: "    provider: remote\n    model: custom\n"},
		{name: "model identity", remote: "    provider: openai\n    model: ${REMOTE_MODEL}\n    base_url: http://remote.test/v1\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			data := []byte("version: \"16\"\nmodels:\n  pick:\n    first_available: [remote, local]\n  remote:\n" + tc.remote + "    context_window: 8192\n  local:\n    provider: openai\n    model: custom\n    base_url: http://local.test/v1\n    context_window: 8192\nagents:\n  root:\n    model: pick\n    instruction: test\n")
			runConfig := &config.RuntimeConfig{
				Config: config.Config{Providers: map[string]latest.ProviderConfig{
					"remote": {Provider: "openai", BaseURL: "${REMOTE_URL}"},
				}},
				ModelsDevStoreOverride: modelsdev.NewDatabaseStore(&modelsdev.Database{}),
				EnvProviderOverride:    environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "test"}),
			}
			calls := 0
			registry := provider.NewRegistry(map[string]provider.Factory{
				"openai": func(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (provider.Provider, error) {
					calls++
					require.Equal(t, "http://local.test/v1", cfg.BaseURL)
					return openai.NewClient(ctx, cfg, env, opts...)
				},
			})
			team, err := LoadWithConfig(t.Context(), config.NewBytesSource("optional-context-window.yaml", data), runConfig, withTestProviderRegistry(WithProviderRegistry(registry))...)
			require.NoError(t, err)
			require.NotNil(t, team)
			require.Equal(t, 1, calls, "only the selected candidate should be constructed")
		})
	}
}
