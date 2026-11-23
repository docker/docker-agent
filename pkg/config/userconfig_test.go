package config

import (
	"math"
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

func TestWithUserConfig_StreamIdleTimeout(t *testing.T) {
	t.Parallel()

	t.Run("applies global default when unset", func(t *testing.T) {
		agentYAML := []byte(`agents:
  root:
    model: openai/gpt-4o
    instruction: test
`)
		cfg, err := Load(t.Context(), NewBytesSource("agent.yaml", agentYAML), WithUserConfig(&userconfig.Config{
			Settings: &userconfig.Settings{
				StreamIdleTimeout: &latest.StreamIdleTimeout{Duration: latest.DisabledStreamIdleTimeout},
			},
		}))
		require.NoError(t, err)
		require.NotNil(t, cfg.Runtime)
		require.NotNil(t, cfg.Runtime.StreamIdleTimeout)
		assert.Equal(t, latest.DisabledStreamIdleTimeout, cfg.Runtime.StreamIdleTimeout.Duration)
	})

	t.Run("agent runtime.stream_idle_timeout wins", func(t *testing.T) {
		agentYAML := []byte(`runtime:
  stream_idle_timeout: 2m
agents:
  root:
    model: openai/gpt-4o
    instruction: test
`)
		cfg, err := Load(t.Context(), NewBytesSource("agent.yaml", agentYAML), WithUserConfig(&userconfig.Config{
			Settings: &userconfig.Settings{
				StreamIdleTimeout: &latest.StreamIdleTimeout{Duration: latest.DisabledStreamIdleTimeout},
			},
		}))
		require.NoError(t, err)
		assert.Equal(t, "2m0s", cfg.Runtime.StreamIdleTimeout.String())
	})
}

func TestWithUserConfig_Compaction(t *testing.T) {
	t.Parallel()

	agentYAML := []byte(`agents:
  root:
    model: openai/gpt-4o
    instruction: test
`)
	userCfg := &userconfig.Config{
		Settings: &userconfig.Settings{
			SessionCompaction:   new(false),
			CompactionThreshold: new(0.8),
			CompactionModel:     "openai/gpt-4o-mini",
		},
	}
	cfg, err := Load(t.Context(), NewBytesSource("agent.yaml", agentYAML), WithUserConfig(userCfg))
	require.NoError(t, err)
	assert.False(t, cfg.Agents[0].SessionCompactionEnabled())
	require.NotNil(t, cfg.Agents[0].CompactionThreshold)
	assert.InDelta(t, 0.8, *cfg.Agents[0].CompactionThreshold, 1e-9)
	assert.Empty(t, cfg.Agents[0].CompactionModel)
	ApplyDeferredUserSettings(cfg, userCfg)
	assert.Equal(t, "openai/gpt-4o-mini", cfg.Agents[0].CompactionModel)
}

func TestWithUserConfig_CompactionModelDeferredForFirstAvailable(t *testing.T) {
	t.Parallel()

	agentYAML := []byte(`models:
  pick:
    first_available:
      - primary
  primary:
    provider: openai
    model: gpt-4o
    compaction_model: openai/gpt-4.1-mini
agents:
  root:
    model: pick
    instruction: test
`)
	userCfg := &userconfig.Config{
		Settings: &userconfig.Settings{CompactionModel: "openai/gpt-4o-mini"},
	}

	cfg, err := Load(t.Context(), NewBytesSource("agent.yaml", agentYAML), WithUserConfig(userCfg))
	require.NoError(t, err)
	// Must not promote global compaction_model onto the agent before first_available resolves.
	assert.Empty(t, cfg.Agents[0].CompactionModel)

	// Simulate first_available resolving "pick" to "primary".
	cfg.Models["pick"] = cfg.Models["primary"]
	ApplyDeferredUserSettings(cfg, userCfg)
	assert.Equal(t, "openai/gpt-4.1-mini", EffectiveCompactionModelRef(cfg, &cfg.Agents[0]))
}

func TestWithUserConfig_BudgetCeiling(t *testing.T) {
	t.Parallel()

	agentYAML := []byte(`budget:
  max_cost: 10.0
  max_tokens: 10000
agents:
  root:
    model: openai/gpt-4o
    instruction: test
`)
	cfg, err := Load(t.Context(), NewBytesSource("agent.yaml", agentYAML), WithUserConfig(&userconfig.Config{
		Settings: &userconfig.Settings{
			Budget: &latest.BudgetConfig{
				MaxCost:   2.0,
				MaxTokens: 50000,
			},
		},
	}))
	require.NoError(t, err)
	require.NotNil(t, cfg.Budget)
	// Global ceiling tightens agent's higher max_cost; agent's stricter max_tokens stays.
	assert.InDelta(t, 2.0, cfg.Budget.MaxCost, 1e-9)
	assert.Equal(t, int64(10000), cfg.Budget.MaxTokens)

	// Non-positive or NaN agent max_cost must not bypass a finite global ceiling.
	merged := mergeBudgetCeiling(&latest.BudgetConfig{MaxCost: math.NaN()}, &latest.BudgetConfig{MaxCost: 2.0})
	assert.InDelta(t, 2.0, merged.MaxCost, 1e-9)
}

func TestWithUserConfig_LoopAndContextLimits(t *testing.T) {
	t.Parallel()

	agentYAML := []byte(`agents:
  root:
    model: openai/gpt-4o
    instruction: test
    max_iterations: 15
`)
	cfg, err := Load(t.Context(), NewBytesSource("agent.yaml", agentYAML), WithUserConfig(&userconfig.Config{
		Settings: &userconfig.Settings{
			MaxIterations:           50,
			MaxConsecutiveToolCalls: 12,
			MaxToolResultTokens:     4000,
			MaxOldToolCallTokens:    2000,
			NumHistoryItems:         40,
		},
	}))
	require.NoError(t, err)
	assert.Equal(t, 15, cfg.Agents[0].MaxIterations)
	assert.Equal(t, 12, cfg.Agents[0].MaxConsecutiveToolCalls)
	assert.Equal(t, 4000, cfg.Agents[0].MaxToolResultTokens)
	assert.Equal(t, 2000, cfg.Agents[0].MaxOldToolCallTokens)
	assert.Equal(t, 40, cfg.Agents[0].NumHistoryItems)
}
