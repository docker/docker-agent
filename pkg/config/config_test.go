package config

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

func TestAutoRegisterModels(t *testing.T) {
	t.Parallel()

	cfg, err := Load(t.Context(), NewFileSource("testdata/autoregister.yaml"))
	require.NoError(t, err)

	assert.Len(t, cfg.Models, 2)
	assert.Equal(t, "openai", cfg.Models["openai/gpt-4o"].Provider)
	assert.Equal(t, "gpt-4o", cfg.Models["openai/gpt-4o"].Model)
	assert.Equal(t, "anthropic", cfg.Models["anthropic/claude-sonnet-4-0"].Provider)
	assert.Equal(t, "claude-sonnet-4-0", cfg.Models["anthropic/claude-sonnet-4-0"].Model)
}

func TestAutoRegisterAlloy(t *testing.T) {
	t.Parallel()

	cfg, err := Load(t.Context(), NewFileSource("testdata/autoregister_alloy.yaml"))
	require.NoError(t, err)

	assert.Len(t, cfg.Models, 2)
	assert.Equal(t, "openai", cfg.Models["openai/gpt-4o"].Provider)
	assert.Equal(t, "gpt-4o", cfg.Models["openai/gpt-4o"].Model)
	assert.Equal(t, "anthropic", cfg.Models["anthropic/claude-sonnet-4-0"].Provider)
	assert.Equal(t, "claude-sonnet-4-0", cfg.Models["anthropic/claude-sonnet-4-0"].Model)
}

func TestAlloyModelComposition(t *testing.T) {
	t.Parallel()

	cfg, err := Load(t.Context(), NewFileSource("testdata/alloy_model_composition.yaml"))
	require.NoError(t, err)

	// The alloy model should be expanded to its constituent models
	assert.Equal(t, "opus,gpt", cfg.Agents.First().Model)

	// The constituent models should still exist
	assert.Equal(t, "anthropic", cfg.Models["opus"].Provider)
	assert.Equal(t, "claude-opus-4-5", cfg.Models["opus"].Model)
	assert.Equal(t, "openai", cfg.Models["gpt"].Provider)
	assert.Equal(t, "gpt-5.2", cfg.Models["gpt"].Model)
}

func TestAlloyModelNestedComposition(t *testing.T) {
	t.Parallel()

	cfg, err := Load(t.Context(), NewFileSource("testdata/alloy_model_nested.yaml"))
	require.NoError(t, err)

	// The nested alloy should be fully expanded to all constituent models
	assert.Equal(t, "opus,gpt,gemini", cfg.Agents.First().Model)

	// All base models should exist
	assert.Equal(t, "anthropic", cfg.Models["opus"].Provider)
	assert.Equal(t, "openai", cfg.Models["gpt"].Provider)
	assert.Equal(t, "google", cfg.Models["gemini"].Provider)
}

func TestMigrate_v0_v1_provider(t *testing.T) {
	t.Parallel()

	cfg, err := Load(t.Context(), NewFileSource("testdata/provider_v0.yaml"))
	require.NoError(t, err)

	assert.Equal(t, "openai", cfg.Models["gpt"].Provider)
}

func TestMigrate_v1_provider(t *testing.T) {
	t.Parallel()

	cfg, err := Load(t.Context(), NewFileSource("testdata/provider_v1.yaml"))
	require.NoError(t, err)

	assert.Equal(t, "openai", cfg.Models["gpt"].Provider)
}

func TestMigrate_v0_v1_todo(t *testing.T) {
	t.Parallel()

	cfg, err := Load(t.Context(), NewFileSource("testdata/todo_v0.yaml"))
	require.NoError(t, err)

	assert.Len(t, cfg.Agents.First().Toolsets, 2)
	assert.Equal(t, "todo", cfg.Agents.First().Toolsets[0].Type)
	assert.False(t, cfg.Agents.First().Toolsets[0].Shared)
	assert.Equal(t, "mcp", cfg.Agents.First().Toolsets[1].Type)
}

func TestMigrate_v1_todo(t *testing.T) {
	t.Parallel()

	cfg, err := Load(t.Context(), NewFileSource("testdata/todo_v1.yaml"))
	require.NoError(t, err)

	assert.Len(t, cfg.Agents.First().Toolsets, 2)
	assert.Equal(t, "todo", cfg.Agents.First().Toolsets[0].Type)
	assert.False(t, cfg.Agents.First().Toolsets[0].Shared)
	assert.Equal(t, "mcp", cfg.Agents.First().Toolsets[1].Type)
}

func TestMigrate_v0_v1_shared_todo(t *testing.T) {
	t.Parallel()

	cfg, err := Load(t.Context(), NewFileSource("testdata/shared_todo_v0.yaml"))
	require.NoError(t, err)

	assert.Len(t, cfg.Agents.First().Toolsets, 2)
	assert.Equal(t, "todo", cfg.Agents.First().Toolsets[0].Type)
	assert.True(t, cfg.Agents.First().Toolsets[0].Shared)
	assert.Equal(t, "mcp", cfg.Agents.First().Toolsets[1].Type)
}

func TestMigrate_v1_shared_todo(t *testing.T) {
	t.Parallel()

	cfg, err := Load(t.Context(), NewFileSource("testdata/shared_todo_v1.yaml"))
	require.NoError(t, err)

	assert.Len(t, cfg.Agents.First().Toolsets, 2)
	assert.Equal(t, "todo", cfg.Agents.First().Toolsets[0].Type)
	assert.True(t, cfg.Agents.First().Toolsets[0].Shared)
	assert.Equal(t, "mcp", cfg.Agents.First().Toolsets[1].Type)
}

func TestMigrate_v0_v1_think(t *testing.T) {
	t.Parallel()

	cfg, err := Load(t.Context(), NewFileSource("testdata/think_v0.yaml"))
	require.NoError(t, err)

	assert.Len(t, cfg.Agents.First().Toolsets, 2)
	assert.Equal(t, "think", cfg.Agents.First().Toolsets[0].Type)
	assert.Equal(t, "mcp", cfg.Agents.First().Toolsets[1].Type)
}

func TestMigrate_v1_think(t *testing.T) {
	t.Parallel()

	cfg, err := Load(t.Context(), NewFileSource("testdata/think_v1.yaml"))
	require.NoError(t, err)

	assert.Len(t, cfg.Agents.First().Toolsets, 2)
	assert.Equal(t, "think", cfg.Agents.First().Toolsets[0].Type)
	assert.Equal(t, "mcp", cfg.Agents.First().Toolsets[1].Type)
}

func TestMigrate_v0_v1_memory(t *testing.T) {
	t.Parallel()

	cfg, err := Load(t.Context(), NewFileSource("testdata/memory_v0.yaml"))
	require.NoError(t, err)

	assert.Len(t, cfg.Agents.First().Toolsets, 2)
	assert.Equal(t, "memory", cfg.Agents.First().Toolsets[0].Type)
	assert.Equal(t, "dev_memory.db", cfg.Agents.First().Toolsets[0].Path)
	assert.Equal(t, "mcp", cfg.Agents.First().Toolsets[1].Type)
}

func TestMigrate_v1_memory(t *testing.T) {
	t.Parallel()

	cfg, err := Load(t.Context(), NewFileSource("testdata/memory_v1.yaml"))
	require.NoError(t, err)

	assert.Len(t, cfg.Agents.First().Toolsets, 2)
	assert.Equal(t, "memory", cfg.Agents.First().Toolsets[0].Type)
	assert.Equal(t, "dev_memory.db", cfg.Agents.First().Toolsets[0].Path)
	assert.Equal(t, "mcp", cfg.Agents.First().Toolsets[1].Type)
}

func TestMigrate_v1(t *testing.T) {
	t.Parallel()

	_, err := Load(t.Context(), NewFileSource("testdata/v1.yaml"))
	require.NoError(t, err)
}

func openRoot(t *testing.T, dir string) *os.Root {
	t.Helper()

	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	t.Cleanup(func() { root.Close() })

	return root
}

type noEnvProvider struct{}

func (p *noEnvProvider) Get(context.Context, string) (string, bool) { return "", false }

type fakeEnvProvider struct {
	vars map[string]string
}

func (p *fakeEnvProvider) Get(_ context.Context, name string) (string, bool) {
	v, ok := p.vars[name]
	return v, ok
}

func TestCheckRequiredEnvVars(t *testing.T) {
	t.Parallel()

	tests := []struct {
		yaml            string
		expectedMissing []string
	}{
		{
			yaml:            "openai_inline.yaml",
			expectedMissing: []string{"OPENAI_API_KEY"},
		},
		{
			yaml:            "anthropic_inline.yaml",
			expectedMissing: []string{"ANTHROPIC_API_KEY"},
		},
		{
			yaml:            "google_inline.yaml",
			expectedMissing: []string{"GOOGLE_API_KEY"},
		},
		{
			yaml:            "mistral_inline.yaml",
			expectedMissing: []string{"MISTRAL_API_KEY"},
		},
		{
			yaml:            "dmr_inline.yaml",
			expectedMissing: []string{},
		},
		{
			yaml:            "openai_model.yaml",
			expectedMissing: []string{"OPENAI_API_KEY"},
		},
		{
			yaml:            "anthropic_model.yaml",
			expectedMissing: []string{"ANTHROPIC_API_KEY"},
		},
		{
			yaml:            "google_model.yaml",
			expectedMissing: []string{"GOOGLE_API_KEY"},
		},
		{
			yaml:            "mistral_model.yaml",
			expectedMissing: []string{"MISTRAL_API_KEY"},
		},
		{
			yaml:            "dmr_model.yaml",
			expectedMissing: []string{},
		},
		{
			yaml:            "all.yaml",
			expectedMissing: []string{"ANTHROPIC_API_KEY", "GOOGLE_API_KEY", "MISTRAL_API_KEY", "OPENAI_API_KEY"},
		},
		{
			yaml:            "openai_and_unused_mistral_model.yaml",
			expectedMissing: []string{"OPENAI_API_KEY"},
		},
	}
	for _, test := range tests {
		t.Run(test.yaml, func(t *testing.T) {
			t.Parallel()

			cfg, err := Load(t.Context(), NewFileSource("testdata/env/"+test.yaml))
			require.NoError(t, err)

			err = CheckRequiredEnvVars(t.Context(), cfg, "", &noEnvProvider{})

			if len(test.expectedMissing) == 0 {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				var reqErr *environment.RequiredEnvError
				require.ErrorAs(t, err, &reqErr)
				assert.Equal(t, test.expectedMissing, reqErr.Missing)
				assert.True(t, reqErr.MissingModelCredentials, "missing vars are model credentials, so the local-model hint must be offered")
			}
		})
	}
}

func TestCheckRequiredEnvVarsWithModelGateway(t *testing.T) {
	t.Parallel()

	t.Run("with token", func(t *testing.T) {
		t.Parallel()

		cfg, err := Load(t.Context(), NewFileSource("testdata/env/all.yaml"))
		require.NoError(t, err)

		env := &fakeEnvProvider{vars: map[string]string{
			environment.DockerDesktopTokenEnv: "some-jwt-token",
		}}

		err = CheckRequiredEnvVars(t.Context(), cfg, "https://models.docker.com", env)
		require.NoError(t, err)
	})

	t.Run("without token", func(t *testing.T) {
		t.Parallel()

		cfg, err := Load(t.Context(), NewFileSource("testdata/env/all.yaml"))
		require.NoError(t, err)

		err = CheckRequiredEnvVars(t.Context(), cfg, "https://models.docker.com", &noEnvProvider{})
		require.ErrorContains(t, err, "sign in Docker Desktop")
	})

	t.Run("non-docker gateway without token", func(t *testing.T) {
		t.Parallel()

		cfg, err := Load(t.Context(), NewFileSource("testdata/env/all.yaml"))
		require.NoError(t, err)

		err = CheckRequiredEnvVars(t.Context(), cfg, "https://my-gateway.example.com", &noEnvProvider{})
		require.NoError(t, err)
	})

	t.Run("localhost gateway without token", func(t *testing.T) {
		t.Parallel()

		cfg, err := Load(t.Context(), NewFileSource("testdata/env/all.yaml"))
		require.NoError(t, err)

		err = CheckRequiredEnvVars(t.Context(), cfg, "http://localhost:8080", &noEnvProvider{})
		require.ErrorContains(t, err, "sign in Docker Desktop")
	})

	t.Run("localhost gateway with token", func(t *testing.T) {
		t.Parallel()

		cfg, err := Load(t.Context(), NewFileSource("testdata/env/all.yaml"))
		require.NoError(t, err)

		env := &fakeEnvProvider{vars: map[string]string{
			environment.DockerDesktopTokenEnv: "some-jwt-token",
		}}

		err = CheckRequiredEnvVars(t.Context(), cfg, "http://localhost:8080", env)
		require.NoError(t, err)
	})
}

func TestCheckRequiredEnvVars_BypassModelsGateway(t *testing.T) {
	t.Parallel()

	cfg := &latest.Config{
		Agents: []latest.AgentConfig{{Name: "root", Model: "direct"}},
		Models: map[string]latest.ModelConfig{
			"direct": {Provider: "anthropic", Model: "claude-sonnet-4-5", BypassModelsGateway: true},
		},
	}

	env := &fakeEnvProvider{vars: map[string]string{
		environment.DockerDesktopTokenEnv: "some-jwt-token",
	}}

	// Even with a gateway configured, a bypassing model dials its provider
	// directly and must have its own credentials present.
	err := CheckRequiredEnvVars(t.Context(), cfg, "https://models.docker.com", env)
	require.Error(t, err)
	var reqErr *environment.RequiredEnvError
	require.ErrorAs(t, err, &reqErr)
	assert.Equal(t, []string{"ANTHROPIC_API_KEY"}, reqErr.Missing)
}

func TestCheckRequiredEnvVars_CustomBaseURLImpliesBypass(t *testing.T) {
	t.Parallel()

	env := &fakeEnvProvider{vars: map[string]string{
		environment.DockerDesktopTokenEnv: "some-jwt-token",
	}}

	t.Run("model base_url requires its token_key", func(t *testing.T) {
		t.Parallel()

		cfg := &latest.Config{
			Agents: []latest.AgentConfig{{Name: "root", Model: "proxied"}},
			Models: map[string]latest.ModelConfig{
				"proxied": {Provider: "openai", Model: "gpt-4o", BaseURL: "https://proxy.example.com/v1", TokenKey: "PROXY_KEY"},
			},
		}

		// A custom base_url implies the gateway bypass, so the model dials its
		// endpoint directly and needs its own credentials.
		err := CheckRequiredEnvVars(t.Context(), cfg, "https://models.docker.com", env)
		require.Error(t, err)
		var reqErr *environment.RequiredEnvError
		require.ErrorAs(t, err, &reqErr)
		assert.Equal(t, []string{"PROXY_KEY"}, reqErr.Missing)
	})

	t.Run("custom provider base_url requires its token_key", func(t *testing.T) {
		t.Parallel()

		cfg := &latest.Config{
			Agents: []latest.AgentConfig{{Name: "root", Model: "custom"}},
			Providers: map[string]latest.ProviderConfig{
				"my_endpoint": {BaseURL: "https://llm.internal.example.com/v1", TokenKey: "MY_TOKEN"},
			},
			Models: map[string]latest.ModelConfig{
				"custom": {Provider: "my_endpoint", Model: "llama-3"},
			},
		}

		err := CheckRequiredEnvVars(t.Context(), cfg, "https://models.docker.com", env)
		require.Error(t, err)
		var reqErr *environment.RequiredEnvError
		require.ErrorAs(t, err, &reqErr)
		assert.Equal(t, []string{"MY_TOKEN"}, reqErr.Missing)
	})

	t.Run("no base_url still relies on the gateway", func(t *testing.T) {
		t.Parallel()

		cfg := &latest.Config{
			Agents: []latest.AgentConfig{{Name: "root", Model: "routed"}},
			Models: map[string]latest.ModelConfig{
				"routed": {Provider: "anthropic", Model: "claude-sonnet-4-5"},
			},
		}

		err := CheckRequiredEnvVars(t.Context(), cfg, "https://models.docker.com", env)
		require.NoError(t, err)
	})

	t.Run("router-level base_url does not bypass the fallback", func(t *testing.T) {
		t.Parallel()

		cfg := &latest.Config{
			Agents: []latest.AgentConfig{{Name: "root", Model: "router"}},
			Models: map[string]latest.ModelConfig{
				"router": {
					Provider: "anthropic",
					Model:    "claude-sonnet-4-5",
					BaseURL:  "https://proxy.example.com/v1",
					TokenKey: "PROXY_KEY",
					Routing:  []latest.RoutingRule{{Model: "openai/gpt-5", Examples: []string{"code"}}},
				},
			},
		}

		// The runtime rebuilds a router's fallback from its "provider/model"
		// spec, dropping the router-level base_url/token_key, so the whole
		// subtree still routes through the gateway.
		err := CheckRequiredEnvVars(t.Context(), cfg, "https://models.docker.com", env)
		require.NoError(t, err)
	})

	t.Run("router on a custom provider bypasses the fallback", func(t *testing.T) {
		t.Parallel()

		cfg := &latest.Config{
			Agents: []latest.AgentConfig{{Name: "root", Model: "router"}},
			Providers: map[string]latest.ProviderConfig{
				"my_endpoint": {BaseURL: "https://llm.internal.example.com/v1", TokenKey: "MY_TOKEN"},
			},
			Models: map[string]latest.ModelConfig{
				"router": {
					Provider: "my_endpoint",
					Model:    "llama-3",
					Routing:  []latest.RoutingRule{{Model: "openai/gpt-5", Examples: []string{"code"}}},
				},
			},
		}

		// The provider name survives the fallback rebuild, so the custom
		// provider's base_url still implies the bypass for the fallback leaf.
		err := CheckRequiredEnvVars(t.Context(), cfg, "https://models.docker.com", env)
		require.Error(t, err)
		var reqErr *environment.RequiredEnvError
		require.ErrorAs(t, err, &reqErr)
		assert.Equal(t, []string{"MY_TOKEN"}, reqErr.Missing)
	})

	t.Run("env refs in inherited provider base_url are required", func(t *testing.T) {
		t.Parallel()

		cfg := &latest.Config{
			Agents: []latest.AgentConfig{{Name: "root", Model: "custom"}},
			Providers: map[string]latest.ProviderConfig{
				"proxy": {BaseURL: "${env.PROXY_URL}", TokenKey: "MY_TOKEN"},
			},
			Models: map[string]latest.ModelConfig{
				"custom": {Provider: "proxy", Model: "llama-3"},
			},
		}

		// The provider-level base_url is merged and expanded at runtime, so
		// its ${env.X} references must be preflighted too.
		err := CheckRequiredEnvVars(t.Context(), cfg, "https://models.docker.com", env)
		require.Error(t, err)
		var reqErr *environment.RequiredEnvError
		require.ErrorAs(t, err, &reqErr)
		assert.Equal(t, []string{"MY_TOKEN", "PROXY_URL"}, reqErr.Missing)
	})
}

func TestCheckRequiredEnvVars_BypassModelsGatewayRouting(t *testing.T) {
	t.Parallel()

	env := &fakeEnvProvider{vars: map[string]string{
		environment.DockerDesktopTokenEnv: "some-jwt-token",
	}}

	t.Run("bypass router propagates to children", func(t *testing.T) {
		t.Parallel()

		cfg := &latest.Config{
			Agents: []latest.AgentConfig{{Name: "root", Model: "router"}},
			Models: map[string]latest.ModelConfig{
				"router": {
					Provider:            "anthropic",
					Model:               "claude-sonnet-4-5",
					BypassModelsGateway: true,
					Routing:             []latest.RoutingRule{{Model: "openai/gpt-5", Examples: []string{"code"}}},
				},
			},
		}

		// The router bypasses, so both the fallback (anthropic) and the routed
		// target (openai) dial directly and need their own credentials.
		err := CheckRequiredEnvVars(t.Context(), cfg, "https://models.docker.com", env)
		require.Error(t, err)
		var reqErr *environment.RequiredEnvError
		require.ErrorAs(t, err, &reqErr)
		assert.Equal(t, []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY"}, reqErr.Missing)
	})

	t.Run("non-bypass router with bypass child requires only the child", func(t *testing.T) {
		t.Parallel()

		cfg := &latest.Config{
			Agents: []latest.AgentConfig{{Name: "root", Model: "router"}},
			Models: map[string]latest.ModelConfig{
				"router": {
					Provider: "anthropic",
					Model:    "claude-sonnet-4-5",
					Routing:  []latest.RoutingRule{{Model: "direct", Examples: []string{"code"}}},
				},
				"direct": {Provider: "openai", Model: "gpt-5", BypassModelsGateway: true},
			},
		}

		// The router and its fallback go through the gateway; only the routed
		// bypass child needs direct credentials.
		err := CheckRequiredEnvVars(t.Context(), cfg, "https://models.docker.com", env)
		require.Error(t, err)
		var reqErr *environment.RequiredEnvError
		require.ErrorAs(t, err, &reqErr)
		assert.Equal(t, []string{"OPENAI_API_KEY"}, reqErr.Missing)
	})
}

func TestApplyModelOverrides(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		agents      []latest.AgentConfig
		overrides   []string
		expected    map[string]string // agent name -> expected model
		expectError bool
		errorMsg    string
	}{
		{
			name: "global override",
			agents: []latest.AgentConfig{
				{Name: "root", Model: "openai/gpt-4"},
				{Name: "other", Model: "anthropic/claude-3"},
			},
			overrides: []string{"google/gemini-pro"},
			expected: map[string]string{
				"root":  "google/gemini-pro",
				"other": "google/gemini-pro",
			},
		},
		{
			name: "single per-agent override",
			agents: []latest.AgentConfig{
				{Name: "root", Model: "openai/gpt-4"},
				{Name: "other", Model: "anthropic/claude-3"},
			},
			overrides: []string{"other=google/gemini-pro"},
			expected: map[string]string{
				"root":  "openai/gpt-4",
				"other": "google/gemini-pro",
			},
		},
		{
			name: "multiple separate flags",
			agents: []latest.AgentConfig{
				{Name: "root", Model: "openai/gpt-4"},
				{Name: "other", Model: "anthropic/claude-3"},
			},
			overrides: []string{"root=openai/gpt-5", "other=anthropic/claude-sonnet-4-0"},
			expected: map[string]string{
				"root":  "openai/gpt-5",
				"other": "anthropic/claude-sonnet-4-0",
			},
		},
		{
			name: "comma-separated format",
			agents: []latest.AgentConfig{
				{Name: "root", Model: "openai/gpt-4"},
				{Name: "other", Model: "anthropic/claude-3"},
				{Name: "third", Model: "google/gemini-pro"},
			},
			overrides: []string{"root=openai/gpt-5,other=anthropic/claude-sonnet-4-0"},
			expected: map[string]string{
				"root":  "openai/gpt-5",
				"other": "anthropic/claude-sonnet-4-0",
				"third": "google/gemini-pro",
			},
		},
		{
			name: "mixed formats",
			agents: []latest.AgentConfig{
				{Name: "root", Model: "openai/gpt-4"},
				{Name: "other", Model: "anthropic/claude-3"},
				{Name: "third", Model: "google/gemini-pro"},
				{Name: "reviewer", Model: "openai/gpt-3.5-turbo"},
			},
			overrides: []string{"root=openai/gpt-5,other=anthropic/claude-4", "reviewer=google/gemini-1.5-pro"},
			expected: map[string]string{
				"root":     "openai/gpt-5",
				"other":    "anthropic/claude-4",
				"third":    "google/gemini-pro",
				"reviewer": "google/gemini-1.5-pro",
			},
		},
		{
			name: "last override wins",
			agents: []latest.AgentConfig{
				{Name: "root", Model: "openai/gpt-4"},
			},
			overrides: []string{"root=openai/gpt-5", "root=anthropic/claude-4"},
			expected: map[string]string{
				"root": "anthropic/claude-4",
			},
		},
		{
			name: "unknown agent error",
			agents: []latest.AgentConfig{
				{Name: "root", Model: "openai/gpt-4"},
			},
			overrides:   []string{"nonexistent=openai/gpt-5"},
			expectError: true,
			errorMsg:    "unknown agent 'nonexistent'",
		},
		{
			name: "empty model spec error",
			agents: []latest.AgentConfig{
				{Name: "root", Model: "openai/gpt-4"},
			},
			overrides:   []string{"root="},
			expectError: true,
			errorMsg:    "empty model specification in override: root=",
		},
		{
			name: "empty global model spec is skipped",
			agents: []latest.AgentConfig{
				{Name: "root", Model: "openai/gpt-4"},
			},
			overrides: []string{""},
			expected: map[string]string{
				"root": "openai/gpt-4",
			},
		},
		{
			name: "whitespace handling",
			agents: []latest.AgentConfig{
				{Name: "root", Model: "openai/gpt-4"},
				{Name: "other", Model: "anthropic/claude-3"},
			},
			overrides: []string{" root = openai/gpt-5 , other = anthropic/claude-4 "},
			expected: map[string]string{
				"root":  "openai/gpt-5",
				"other": "anthropic/claude-4",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := &latest.Config{
				Agents: tt.agents,
				Models: make(map[string]latest.ModelConfig),
			}

			err := ApplyModelOverrides(cfg, tt.overrides)

			if tt.expectError {
				require.ErrorContains(t, err, tt.errorMsg)
			} else {
				require.NoError(t, err)
				for _, agent := range cfg.Agents {
					assert.Equal(t, tt.expected[agent.Name], agent.Model, "wrong model for agent %s", agent.Name)
				}
			}
		})
	}
}

func TestValidateConfig_HarnessAgentsSkipModelValidation(t *testing.T) {
	t.Parallel()

	cfg := &latest.Config{
		Agents: []latest.AgentConfig{
			{Name: "root", Model: "openai/gpt-4o", SubAgents: []string{"coder"}},
			{Name: "coder", Harness: &latest.HarnessConfig{Type: "codex"}},
		},
	}

	require.NoError(t, validateConfig(cfg))
	_, exists := cfg.Models[""]
	assert.False(t, exists)
}

func TestValidateConfig_HarnessValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		harness *latest.HarnessConfig
		wantErr string
	}{
		{name: "valid claude code", harness: &latest.HarnessConfig{Type: "claude-code", Effort: "high"}},
		{name: "missing type", harness: &latest.HarnessConfig{}, wantErr: "harness.type is required"},
		{name: "bad type", harness: &latest.HarnessConfig{Type: "vim"}, wantErr: "unsupported harness.type"},
		{name: "bad effort", harness: &latest.HarnessConfig{Type: "claude-code", Effort: "ultra"}, wantErr: "harness.effort must be one of"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := &latest.Config{Agents: []latest.AgentConfig{{Name: "root", Harness: tt.harness}}}
			err := cfg.Validate()
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestValidateConfig_ExternalSubAgentReferences(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     *latest.Config
		wantErr string
	}{
		{
			name: "OCI reference in sub_agents is allowed",
			cfg: &latest.Config{
				Agents: []latest.AgentConfig{
					{Name: "root", Model: "openai/gpt-4o", SubAgents: []string{"myorg/agent:tag"}},
				},
			},
		},
		{
			name: "OCI reference with tag in sub_agents is allowed",
			cfg: &latest.Config{
				Agents: []latest.AgentConfig{
					{Name: "root", Model: "openai/gpt-4o", SubAgents: []string{"docker.io/myorg/myagent:v1"}},
				},
			},
		},
		{
			name: "mix of local and external sub_agents",
			cfg: &latest.Config{
				Agents: []latest.AgentConfig{
					{Name: "root", Model: "openai/gpt-4o", SubAgents: []string{"helper", "myorg/agent:tag"}},
					{Name: "helper", Model: "openai/gpt-4o"},
				},
			},
		},
		{
			name: "non-existent local sub_agent still fails",
			cfg: &latest.Config{
				Agents: []latest.AgentConfig{
					{Name: "root", Model: "openai/gpt-4o", SubAgents: []string{"does_not_exist"}},
				},
			},
			wantErr: "non-existent sub-agent 'does_not_exist'",
		},
		{
			name: "URL reference in sub_agents is allowed",
			cfg: &latest.Config{
				Agents: []latest.AgentConfig{
					{Name: "root", Model: "openai/gpt-4o", SubAgents: []string{"https://example.com/agent.yaml"}},
				},
			},
		},
		{
			name: "OCI reference in handoffs is allowed",
			cfg: &latest.Config{
				Agents: []latest.AgentConfig{
					{Name: "root", Model: "openai/gpt-4o", Handoffs: []string{"myorg/agent:tag"}},
				},
			},
		},
		{
			name: "non-existent local handoff fails",
			cfg: &latest.Config{
				Agents: []latest.AgentConfig{
					{Name: "root", Model: "openai/gpt-4o", Handoffs: []string{"does_not_exist"}},
				},
			},
			wantErr: "non-existent handoff agent 'does_not_exist'",
		},
		{
			name: "named OCI reference in sub_agents is allowed",
			cfg: &latest.Config{
				Agents: []latest.AgentConfig{
					{Name: "root", Model: "openai/gpt-4o", SubAgents: []string{"reviewer:myorg/review-pr"}},
				},
			},
		},
		{
			name: "named URL reference in sub_agents is allowed",
			cfg: &latest.Config{
				Agents: []latest.AgentConfig{
					{Name: "root", Model: "openai/gpt-4o", SubAgents: []string{"myagent:https://example.com/agent.yaml"}},
				},
			},
		},
		{
			name: "named OCI reference in handoffs is allowed",
			cfg: &latest.Config{
				Agents: []latest.AgentConfig{
					{Name: "root", Model: "openai/gpt-4o", Handoffs: []string{"reviewer:myorg/review-pr"}},
				},
			},
		},
		{
			name: "external sub-agent name collides with local agent",
			cfg: &latest.Config{
				Agents: []latest.AgentConfig{
					{Name: "root", Model: "openai/gpt-4o", SubAgents: []string{"agent", "myorg/agent:tag"}},
					{Name: "agent", Model: "openai/gpt-4o"},
				},
			},
			wantErr: "conflicts with a locally-defined agent",
		},
		{
			name: "named external sub-agent collides with local agent",
			cfg: &latest.Config{
				Agents: []latest.AgentConfig{
					{Name: "root", Model: "openai/gpt-4o", SubAgents: []string{"helper", "helper:myorg/review-pr"}},
					{Name: "helper", Model: "openai/gpt-4o"},
				},
			},
			wantErr: "conflicts with a locally-defined agent",
		},
		{
			name: "external handoff name collides with local agent",
			cfg: &latest.Config{
				Agents: []latest.AgentConfig{
					{Name: "root", Model: "openai/gpt-4o", Handoffs: []string{"myorg/agent:tag"}},
					{Name: "agent", Model: "openai/gpt-4o"},
				},
			},
			wantErr: "conflicts with a locally-defined agent",
		},
		{
			name: "local handoff to another agent passes",
			cfg: &latest.Config{
				Agents: []latest.AgentConfig{
					{Name: "root", Model: "openai/gpt-4o", Handoffs: []string{"helper"}},
					{Name: "helper", Model: "openai/gpt-4o"},
				},
			},
		},
		{
			name: "force_handoff to another local agent passes",
			cfg: &latest.Config{
				Agents: []latest.AgentConfig{
					{Name: "root", Model: "openai/gpt-4o", ForceHandoff: "helper"},
					{Name: "helper", Model: "openai/gpt-4o"},
				},
			},
		},
		{
			name: "force_handoff chain passes",
			cfg: &latest.Config{
				Agents: []latest.AgentConfig{
					{Name: "a", Model: "openai/gpt-4o", ForceHandoff: "b"},
					{Name: "b", Model: "openai/gpt-4o", ForceHandoff: "c"},
					{Name: "c", Model: "openai/gpt-4o"},
				},
			},
		},
		{
			name: "force_handoff to non-existent agent fails",
			cfg: &latest.Config{
				Agents: []latest.AgentConfig{
					{Name: "root", Model: "openai/gpt-4o", ForceHandoff: "does_not_exist"},
				},
			},
			wantErr: "non-existent force_handoff agent 'does_not_exist'",
		},
		{
			name: "force_handoff to self fails",
			cfg: &latest.Config{
				Agents: []latest.AgentConfig{
					{Name: "root", Model: "openai/gpt-4o", ForceHandoff: "root"},
				},
			},
			wantErr: "cannot force_handoff to itself",
		},
		{
			name: "force_handoff cycle fails",
			cfg: &latest.Config{
				Agents: []latest.AgentConfig{
					{Name: "a", Model: "openai/gpt-4o", ForceHandoff: "b"},
					{Name: "b", Model: "openai/gpt-4o", ForceHandoff: "a"},
				},
			},
			wantErr: "force_handoff cycle detected",
		},
		{
			name: "force_handoff three-agent cycle fails",
			cfg: &latest.Config{
				Agents: []latest.AgentConfig{
					{Name: "a", Model: "openai/gpt-4o", ForceHandoff: "b"},
					{Name: "b", Model: "openai/gpt-4o", ForceHandoff: "c"},
					{Name: "c", Model: "openai/gpt-4o", ForceHandoff: "a"},
				},
			},
			wantErr: "force_handoff cycle detected",
		},
		{
			name: "external force_handoff reference is allowed",
			cfg: &latest.Config{
				Agents: []latest.AgentConfig{
					{Name: "root", Model: "openai/gpt-4o", ForceHandoff: "myorg/agent:tag"},
				},
			},
		},
		{
			name: "external force_handoff name collides with local agent",
			cfg: &latest.Config{
				Agents: []latest.AgentConfig{
					{Name: "root", Model: "openai/gpt-4o", ForceHandoff: "myorg/agent:tag"},
					{Name: "agent", Model: "openai/gpt-4o"},
				},
			},
			wantErr: "conflicts with a locally-defined agent",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := validateConfig(tt.cfg)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestProviders_Validation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		providers map[string]latest.ProviderConfig
		wantErr   string
	}{
		{
			name: "valid provider",
			providers: map[string]latest.ProviderConfig{
				"my_provider": {
					APIType:  "openai_chatcompletions",
					BaseURL:  "https://api.example.com/v1",
					TokenKey: "MY_API_KEY",
				},
			},
			wantErr: "",
		},
		{
			name: "valid provider with responses api_type",
			providers: map[string]latest.ProviderConfig{
				"responses_provider": {
					APIType: "openai_responses",
					BaseURL: "https://api.example.com/v1",
				},
			},
			wantErr: "",
		},
		{
			name: "valid provider with empty api_type",
			providers: map[string]latest.ProviderConfig{
				"default_provider": {
					BaseURL: "https://api.example.com/v1",
				},
			},
			wantErr: "",
		},
		{
			name: "missing base_url",
			providers: map[string]latest.ProviderConfig{
				"no_base_url": {
					APIType: "openai_chatcompletions",
				},
			},
			wantErr: "base_url is required",
		},
		{
			name: "invalid api_type",
			providers: map[string]latest.ProviderConfig{
				"bad_provider": {
					APIType: "invalid_api_type",
					BaseURL: "https://api.example.com/v1",
				},
			},
			wantErr: "invalid api_type 'invalid_api_type'",
		},
		{
			name: "provider name with slash",
			providers: map[string]latest.ProviderConfig{
				"bad/name": {
					APIType: "openai_chatcompletions",
					BaseURL: "https://api.example.com/v1",
				},
			},
			wantErr: "name cannot contain '/'",
		},
		{
			name: "valid anthropic provider without base_url",
			providers: map[string]latest.ProviderConfig{
				"my_anthropic": {
					Provider: "anthropic",
					TokenKey: "MY_ANTHROPIC_KEY",
				},
			},
			wantErr: "",
		},
		{
			name: "valid google provider with defaults",
			providers: map[string]latest.ProviderConfig{
				"my_google": {
					Provider: "google",
				},
			},
			wantErr: "",
		},
		{
			name: "openai provider without base_url requires it",
			providers: map[string]latest.ProviderConfig{
				"my_openai": {
					Provider: "openai",
				},
			},
			wantErr: "base_url is required",
		},
		{
			name: "provider with model defaults",
			providers: map[string]latest.ProviderConfig{
				"my_anthropic": {
					Provider:    "anthropic",
					TokenKey:    "MY_KEY",
					MaxTokens:   new(int64),
					Temperature: new(float64),
				},
			},
			wantErr: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := &latest.Config{
				Providers: tt.providers,
				Agents: []latest.AgentConfig{
					{Name: "root", Model: "openai/gpt-4o"},
				},
			}

			err := validateConfig(cfg)

			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestMergeGlobalProviders(t *testing.T) {
	t.Parallel()

	global := map[string]latest.ProviderConfig{
		"myprovider": {BaseURL: "https://global.example.com/v1", TokenKey: "MY_KEY"},
		"other":      {BaseURL: "https://other.example.com/v1"},
	}

	t.Run("adds global providers", func(t *testing.T) {
		t.Parallel()

		cfg := &latest.Config{}
		MergeGlobalProviders(cfg, global)

		require.Len(t, cfg.Providers, 2)
		assert.Equal(t, "https://global.example.com/v1", cfg.Providers["myprovider"].BaseURL)
	})

	t.Run("agent file definition wins", func(t *testing.T) {
		t.Parallel()

		cfg := &latest.Config{Providers: map[string]latest.ProviderConfig{
			"myprovider": {BaseURL: "https://file.example.com/v1"},
		}}
		MergeGlobalProviders(cfg, global)

		assert.Equal(t, "https://file.example.com/v1", cfg.Providers["myprovider"].BaseURL)
		assert.Equal(t, "https://other.example.com/v1", cfg.Providers["other"].BaseURL)
	})

	t.Run("invalid global provider is skipped", func(t *testing.T) {
		t.Parallel()

		cfg := &latest.Config{}
		MergeGlobalProviders(cfg, map[string]latest.ProviderConfig{
			"no-base-url": {APIType: "openai_chatcompletions"},
			"valid":       {BaseURL: "https://ok.example.com/v1"},
		})

		require.Len(t, cfg.Providers, 1)
		assert.Contains(t, cfg.Providers, "valid")
	})

	t.Run("no global providers leaves config untouched", func(t *testing.T) {
		t.Parallel()

		cfg := &latest.Config{}
		MergeGlobalProviders(cfg, nil)
		assert.Nil(t, cfg.Providers)
	})
}

func TestLoadUnknownKeyFromNewerVersionHint(t *testing.T) {
	t.Parallel()

	// instruction_file was introduced in version 11.
	cfgStr := `version: "10"
agents:
  root:
    model: openai/gpt-4o
    instruction_file: prompt.md
`
	_, err := Load(t.Context(), NewBytesSource("test.yaml", []byte(cfgStr)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown field "instruction_file"`)
	assert.Contains(t, err.Error(), "supported by config version 11")
	assert.Contains(t, err.Error(), "currently 10")
}

func TestLoadUnknownKeyNoNewerVersionNoHint(t *testing.T) {
	t.Parallel()

	cfgStr := `version: "` + latest.Version + `"
agents:
  root:
    model: openai/gpt-4o
    instruction: test
    not_a_real_key: true
`
	_, err := Load(t.Context(), NewBytesSource("test.yaml", []byte(cfgStr)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown field "not_a_real_key"`)
	assert.NotContains(t, err.Error(), "hint:")
}

// TestLoadRemovedFieldHint pins the fix for issue #4175: a version-less (or
// explicit latest-version) config with a shell toolset's `safer: true` still
// fails — the field was intentionally removed as of config v15 (PR #4169)
// and stays removed — but the error now points out that `safer` used to be
// part of config version 14 and should be deleted, instead of silently
// suggesting a `version:` downgrade the way the newer-version hint's wording
// would.
func TestLoadRemovedFieldHint(t *testing.T) {
	t.Parallel()

	cfgStr := `agents:
  root:
    model: openai/gpt-4o
    instruction: test
    toolsets:
      - type: shell
        safer: true
`
	_, err := Load(t.Context(), NewBytesSource("test.yaml", []byte(cfgStr)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown field "safer"`)
	assert.Contains(t, err.Error(), "config version 14")
	assert.Contains(t, err.Error(), "delete it from your config")
}

func TestLoadNewerVersionHintNumericOrdering(t *testing.T) {
	t.Parallel()

	// force_handoff was introduced in version 10. Declaring version "2" pins
	// two behaviors: versions compare numerically ("10" > "2" despite
	// lexicographic order) and the smallest accepting version wins (10, not
	// 11 or 12).
	cfgStr := `version: "2"
agents:
  root:
    model: openai/gpt-4o
    instruction: test
    force_handoff: root
`
	_, err := Load(t.Context(), NewBytesSource("test.yaml", []byte(cfgStr)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown field "force_handoff"`)
	assert.Contains(t, err.Error(), "supported by config version 10")
	assert.Contains(t, err.Error(), "currently 2")
}
