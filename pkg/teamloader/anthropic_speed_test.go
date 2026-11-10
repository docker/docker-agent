package teamloader

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

func userConfigRunConfig(providers map[string]latest.ProviderConfig, defaultModel *latest.ModelConfig) *config.RuntimeConfig {
	return &config.RuntimeConfig{
		Config:                 config.Config{Providers: providers, DefaultModel: defaultModel},
		EnvProviderOverride:    environment.NewMapEnvProvider(map[string]string{"ANTHROPIC_API_KEY": "test-key"}),
		ModelsDevStoreOverride: modelsdev.NewDatabaseStore(&modelsdev.Database{}),
	}
}

func fastProvider() latest.ProviderConfig {
	return latest.ProviderConfig{Provider: "anthropic", ProviderOpts: map[string]any{"speed": "fast"}}
}

func TestLoadWithConfig_AppliesUserConfigAnthropicSpeed(t *testing.T) {
	t.Parallel()

	autoAgent := []byte("agents:\n  root:\n    model: auto\n    instruction: test\n")
	directAgent := []byte("agents:\n  root:\n    model: anthropic/claude-opus-5-5\n    instruction: test\n")
	namedAgent := []byte("models:\n  m:\n    provider: anthropic\n    model: claude-opus-5-5\nagents:\n  root:\n    model: m\n    instruction: test\n")
	standardAgent := []byte("models:\n  m:\n    provider: anthropic\n    model: claude-opus-5-5\n    provider_opts:\n      speed: standard\nagents:\n  root:\n    model: m\n    instruction: test\n")

	for name, tc := range map[string]struct {
		agent     []byte
		providers map[string]latest.ProviderConfig
		defModel  *latest.ModelConfig
		overrides []string
		want      any
	}{
		"custom provider selected with --model": {
			agent: autoAgent, providers: map[string]latest.ProviderConfig{"fast-claude": fastProvider()},
			overrides: []string{"fast-claude/claude-opus-5-5"}, want: "fast",
		},
		"built-in provider name applies to provider/model refs": {
			agent: directAgent, providers: map[string]latest.ProviderConfig{"anthropic": fastProvider()}, want: "fast",
		},
		"built-in provider name applies to named models": {
			agent: namedAgent, providers: map[string]latest.ProviderConfig{"anthropic": fastProvider()}, want: "fast",
		},
		"model-level standard overrides the global fast": {
			agent: standardAgent, providers: map[string]latest.ProviderConfig{"anthropic": fastProvider()}, want: "standard",
		},
		"default_model applies to auto": {
			agent:    autoAgent,
			defModel: &latest.ModelConfig{Provider: "anthropic", Model: "claude-opus-5-5", ProviderOpts: map[string]any{"speed": "fast"}},
			want:     "fast",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			result, err := LoadWithConfig(t.Context(), config.NewBytesSource("agent.yaml", tc.agent), userConfigRunConfig(tc.providers, tc.defModel),
				withTestProviderRegistry(WithModelOverrides(tc.overrides))...)
			require.NoError(t, err)

			a, err := result.Team.Agent("root")
			require.NoError(t, err)
			models := a.ConfiguredModels()
			require.Len(t, models, 1)
			got := models[0].BaseConfig().ModelConfig
			assert.Equal(t, "anthropic", got.Provider)
			assert.Equal(t, tc.want, got.ProviderOpts["speed"])
		})
	}
}

func TestLoadWithConfig_RejectsInvalidUserConfigAnthropicSpeed(t *testing.T) {
	t.Parallel()

	const wantError = "anthropic: speed must be fast or standard"
	invalid := map[string]any{"speed": "turbo"}

	t.Run("providers entry", func(t *testing.T) {
		t.Parallel()
		agent := []byte("agents:\n  root:\n    model: anthropic/claude-opus-5-5\n    instruction: test\n")
		providers := map[string]latest.ProviderConfig{"anthropic": {Provider: "anthropic", ProviderOpts: invalid}}

		_, err := LoadWithConfig(t.Context(), config.NewBytesSource("agent.yaml", agent), userConfigRunConfig(providers, nil), withTestProviderRegistry()...)
		require.ErrorContains(t, err, wantError)
	})

	t.Run("default_model", func(t *testing.T) {
		t.Parallel()
		agent := []byte("agents:\n  root:\n    model: auto\n    instruction: test\n")
		defaultModel := &latest.ModelConfig{Provider: "anthropic", Model: "claude-opus-5-5", ProviderOpts: invalid}

		_, err := LoadWithConfig(t.Context(), config.NewBytesSource("agent.yaml", agent), userConfigRunConfig(nil, defaultModel), withTestProviderRegistry()...)
		var autoErr *config.AutoModelFallbackError
		require.ErrorAs(t, err, &autoErr)
		require.ErrorContains(t, err, wantError)
	})
}
