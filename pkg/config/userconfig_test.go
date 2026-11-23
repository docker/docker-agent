package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func TestWithUserConfig_MergesProviders(t *testing.T) {
	t.Parallel()

	agentYAML := []byte(`agents:
  root:
    model: corp/my-model
    instruction: test
`)
	userCfg := &userconfig.Config{
		Providers: map[string]latest.ProviderConfig{
			"corp": {
				Provider: "openai",
				BaseURL:  "https://llm.corp.internal/v1",
				TokenKey: "CORP_API_KEY",
			},
		},
	}

	cfg, err := Load(t.Context(), NewBytesSource("agent.yaml", agentYAML), WithUserConfig(userCfg))
	require.NoError(t, err)
	require.Contains(t, cfg.Providers, "corp")
	assert.Equal(t, "https://llm.corp.internal/v1", cfg.Providers["corp"].BaseURL)
}

func TestWithUserConfig_NilIsNoop(t *testing.T) {
	t.Parallel()

	agentYAML := []byte(`agents:
  root:
    model: openai/gpt-4o
    instruction: test
`)
	cfg, err := Load(t.Context(), NewBytesSource("agent.yaml", agentYAML), WithUserConfig(nil))
	require.NoError(t, err)
	assert.Len(t, cfg.Agents, 1)
}

func TestWithUserConfig_TitleModel(t *testing.T) {
	t.Parallel()

	agentYAML := []byte(`models:
  primary:
    provider: anthropic
    model: claude-sonnet-4-5
    title_model: openai/gpt-4o
agents:
  root:
    model: primary
    instruction: test
`)
	userCfg := &userconfig.Config{
		Settings: &userconfig.Settings{TitleModel: "openai/gpt-4o-mini"},
	}

	cfg, err := Load(t.Context(), NewBytesSource("agent.yaml", agentYAML), WithUserConfig(userCfg))
	require.NoError(t, err)
	assert.Equal(t, "openai/gpt-4o-mini", cfg.Models["primary"].TitleModel)
}

func TestWithUserConfig_RedactSecrets(t *testing.T) {
	t.Parallel()

	t.Run("global true overrides agent false", func(t *testing.T) {
		agentYAML := []byte(`agents:
  root:
    model: openai/gpt-4o
    instruction: test
    redact_secrets: false
`)
		cfg, err := Load(t.Context(), NewBytesSource("agent.yaml", agentYAML), WithUserConfig(&userconfig.Config{
			Settings: &userconfig.Settings{RedactSecrets: new(true)},
		}))
		require.NoError(t, err)
		assert.True(t, cfg.Agents[0].RedactSecretsEnabled())
	})

	t.Run("global false disables default when unset", func(t *testing.T) {
		agentYAML := []byte(`agents:
  root:
    model: openai/gpt-4o
    instruction: test
`)
		cfg, err := Load(t.Context(), NewBytesSource("agent.yaml", agentYAML), WithUserConfig(&userconfig.Config{
			Settings: &userconfig.Settings{RedactSecrets: new(false)},
		}))
		require.NoError(t, err)
		assert.False(t, cfg.Agents[0].RedactSecretsEnabled())
	})

	t.Run("agent true tightens global false", func(t *testing.T) {
		agentYAML := []byte(`agents:
  root:
    model: openai/gpt-4o
    instruction: test
    redact_secrets: true
`)
		cfg, err := Load(t.Context(), NewBytesSource("agent.yaml", agentYAML), WithUserConfig(&userconfig.Config{
			Settings: &userconfig.Settings{RedactSecrets: new(false)},
		}))
		require.NoError(t, err)
		assert.True(t, cfg.Agents[0].RedactSecretsEnabled())
	})
}

func TestWithUserConfig_SudoAskpass(t *testing.T) {
	t.Parallel()

	t.Run("global false overrides toolset true", func(t *testing.T) {
		agentYAML := []byte(`agents:
  root:
    model: openai/gpt-4o
    instruction: test
    toolsets:
      - type: shell
        sudo_askpass: true
`)
		cfg, err := Load(t.Context(), NewBytesSource("agent.yaml", agentYAML), WithUserConfig(&userconfig.Config{
			Settings: &userconfig.Settings{SudoAskpass: new(false)},
		}))
		require.NoError(t, err)
		require.NotNil(t, cfg.Agents[0].Toolsets[0].SudoAskpass)
		assert.False(t, *cfg.Agents[0].Toolsets[0].SudoAskpass)
	})

	t.Run("global true enables default on shell only", func(t *testing.T) {
		agentYAML := []byte(`agents:
  root:
    model: openai/gpt-4o
    instruction: test
    toolsets:
      - type: shell
      - type: filesystem
`)
		cfg, err := Load(t.Context(), NewBytesSource("agent.yaml", agentYAML), WithUserConfig(&userconfig.Config{
			Settings: &userconfig.Settings{SudoAskpass: new(true)},
		}))
		require.NoError(t, err)
		require.NotNil(t, cfg.Agents[0].Toolsets[0].SudoAskpass)
		assert.True(t, *cfg.Agents[0].Toolsets[0].SudoAskpass)
		assert.Nil(t, cfg.Agents[0].Toolsets[1].SudoAskpass)
	})
}

func TestWithUserConfig_Sandbox(t *testing.T) {
	t.Parallel()

	agentYAML := []byte(`agents:
  root:
    model: openai/gpt-4o
    instruction: test
`)
	cfg, err := Load(t.Context(), NewBytesSource("agent.yaml", agentYAML), WithUserConfig(&userconfig.Config{
		Settings: &userconfig.Settings{Sandbox: true},
	}))
	require.NoError(t, err)
	require.NotNil(t, cfg.Runtime)
	assert.True(t, cfg.Runtime.Sandbox)
}
