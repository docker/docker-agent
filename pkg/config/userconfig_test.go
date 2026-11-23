package config

import (
	"math"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/config/legacy"
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
	assert.Equal(t, "openai/gpt-4o-mini", EffectiveTitleModelRef(cfg, &cfg.Agents[0]))
	exported, err := yaml.Marshal(cfg)
	require.NoError(t, err)
	reloaded, err := Load(t.Context(), NewBytesSource("exported.yaml", exported))
	require.NoError(t, err)
	assert.Equal(t, cfg.DefaultTitleModel, EffectiveTitleModelRef(reloaded, &reloaded.Agents[0]))
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
	assert.Equal(t, "openai/gpt-4o-mini", EffectiveCompactionModelRef(cfg, &cfg.Agents[0]))
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

func TestWithUserConfig_CodeModeTools(t *testing.T) {
	t.Parallel()

	agentYAML := []byte(`agents:
  root:
    model: openai/gpt-4o
    instruction: test
`)
	cfg, err := Load(t.Context(), NewBytesSource("agent.yaml", agentYAML), WithUserConfig(&userconfig.Config{
		Settings: &userconfig.Settings{CodeModeTools: true},
	}))
	require.NoError(t, err)
	assert.True(t, cfg.Agents[0].CodeModeTools)
}

func TestWithUserConfig_Evaluators(t *testing.T) {
	t.Parallel()

	agentYAML := []byte(`agents:
  root:
    model: openai/gpt-4o
    instruction: test
    hooks:
      tool_guard:
        - matcher: "shell"
          hooks:
            - type: evaluator
              evaluator: risk
              evaluator_policy:
                min_probability: 0.7
                fallback: ask
                decisions:
                  "true": deny
                  "false": allow
`)
	userCfg := &userconfig.Config{
		Evaluators: map[string]latest.EvaluatorConfig{
			"risk": {
				Provider:     "openai",
				Model:        "gpt-4o-mini",
				Type:         "boolean",
				Instructions: "Flag risky shell commands",
			},
		},
	}

	cfg, err := Load(t.Context(), NewBytesSource("agent.yaml", agentYAML), WithUserConfig(userCfg))
	require.NoError(t, err)
	require.Contains(t, cfg.Evaluators, "risk")
	assert.Equal(t, "gpt-4o-mini", cfg.Evaluators["risk"].Model)

	// Post-merge validation must also run for historical agent config versions.
	v15YAML := []byte("version: \"15\"\nagents:\n  root:\n    model: bad\n    instruction: test\n")
	_, err = Load(t.Context(), NewBytesSource("v15.yaml", v15YAML),
		WithVersions(legacy.Register),
		WithUserConfig(&userconfig.Config{
			Models: map[string]latest.ModelConfig{
				"bad": {Provider: "openai", Model: "gpt-4o", CompactionThreshold: new(1.5)},
			},
		}),
	)
	require.ErrorContains(t, err, "compaction_threshold")
}

func TestWithUserConfig_Models(t *testing.T) {
	t.Parallel()

	agentYAML := []byte(`agents:
  root:
    model: fast
    instruction: test
`)
	userCfg := &userconfig.Config{
		Models: map[string]latest.ModelConfig{
			"fast": {
				Provider: "openai",
				Model:    "gpt-4o-mini",
			},
		},
	}

	cfg, err := Load(t.Context(), NewBytesSource("agent.yaml", agentYAML), WithUserConfig(userCfg))
	require.NoError(t, err)
	require.Contains(t, cfg.Models, "fast")
	assert.Equal(t, "gpt-4o-mini", cfg.Models["fast"].Model)
}

func TestWithUserConfig_LegacyProviderAuth(t *testing.T) {
	t.Parallel()
	source := NewBytesSource("legacy.yaml", []byte(`version: "15"
models:
  main:
    provider: corp
    model: claude-sonnet-4-5
    auth:
      type: workload_identity_federation
      workload_identity_federation:
        federation_rule_id: fdrl_test
        organization_id: org-test
        identity_token:
          env: CORP_IDENTITY_TOKEN
agents:
  manager:
    model: main
    instruction: test
  helper:
    model: main
    instruction: help
`))
	cfg, err := Load(t.Context(), source, WithVersions(legacy.Register), WithUserConfig(&userconfig.Config{
		Providers: map[string]latest.ProviderConfig{"corp": {Provider: "anthropic", BaseURL: "https://corp.example.com"}},
	}))
	require.NoError(t, err)
	assert.Equal(t, "https://corp.example.com", cfg.Providers["corp"].BaseURL)
	assert.Equal(t, "manager", cfg.Agents.First().Name)
}

func TestWithUserConfig_MCPs(t *testing.T) {
	t.Parallel()

	agentYAML := []byte(`agents:
  root:
    model: openai/gpt-4o
    instruction: test
    toolsets:
      - type: mcp
        ref: github
`)
	userCfg := &userconfig.Config{
		MCPs: map[string]latest.MCPToolset{
			"github": {
				Toolset: latest.Toolset{
					Type:    "mcp",
					Command: "npx",
					Args:    []string{"-y", "@modelcontextprotocol/server-github"},
				},
			},
		},
	}

	cfg, err := Load(t.Context(), NewBytesSource("agent.yaml", agentYAML), WithUserConfig(userCfg))
	require.NoError(t, err)
	require.Len(t, cfg.Agents[0].Toolsets, 1)
	assert.Equal(t, "npx", cfg.Agents[0].Toolsets[0].Command)
	assert.Equal(t, []string{"-y", "@modelcontextprotocol/server-github"}, cfg.Agents[0].Toolsets[0].Args)
}

func TestWithUserConfig_ToolPolicies(t *testing.T) {
	t.Parallel()

	agentYAML := []byte(`agents:
  root:
    model: openai/gpt-4o
    instruction: test
    toolsets:
      - type: fetch
        allow_private_ips: true
        blocked_domains: ["agent-blocked.example"]
      - type: filesystem
        deny_list: ["/etc"]
      - type: mcp_catalog
        blocked_servers: ["legacy"]
      - type: mcp
        ref: docker:context7
        allow_private_ips: true
`)
	userCfg := &userconfig.Config{
		Settings: &userconfig.Settings{
			Tools: &userconfig.ToolPolicies{
				AllowPrivateIPs: new(false),
				Filesystem: &userconfig.FilesystemPolicy{
					AllowList: []string{"."},
					DenyList:  []string{"~/.ssh", "/etc"},
				},
				Fetch: &userconfig.FetchPolicy{
					BlockedDomains: []string{"169.254.169.254"},
				},
				MCPCatalog: &userconfig.MCPCatalogPolicy{
					AllowedServers: []string{"github", "postgres"},
					BlockedServers: []string{"untrusted"},
				},
			},
		},
	}

	cfg, err := Load(t.Context(), NewBytesSource("agent.yaml", agentYAML), WithUserConfig(userCfg))
	require.NoError(t, err)

	fetchTS := cfg.Agents[0].Toolsets[0]
	require.NotNil(t, fetchTS.AllowPrivateIPs)
	assert.False(t, *fetchTS.AllowPrivateIPs, "global allow_private_ips: false must override agent true")
	assert.Equal(t, []string{"agent-blocked.example", "169.254.169.254"}, fetchTS.BlockedDomains)

	fsTS := cfg.Agents[0].Toolsets[1]
	assert.Equal(t, []string{"."}, fsTS.AllowList)
	assert.Equal(t, []string{"/etc", "~/.ssh"}, fsTS.DenyList)

	catTS := cfg.Agents[0].Toolsets[2]
	assert.Equal(t, []string{"github", "postgres"}, catTS.AllowedServers)
	assert.Equal(t, []string{"legacy", "untrusted"}, catTS.BlockedServers)

	mcpRefTS := cfg.Agents[0].Toolsets[3]
	require.NotNil(t, mcpRefTS.AllowPrivateIPs)
	assert.False(t, *mcpRefTS.AllowPrivateIPs, "global allow_private_ips: false must override docker: MCP ref true")
}
