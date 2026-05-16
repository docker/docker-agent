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

		err = CheckRequiredEnvVars(t.Context(), cfg, "gateway:8080", env)
		require.NoError(t, err)
	})

	t.Run("without token", func(t *testing.T) {
		t.Parallel()

		cfg, err := Load(t.Context(), NewFileSource("testdata/env/all.yaml"))
		require.NoError(t, err)

		err = CheckRequiredEnvVars(t.Context(), cfg, "gateway:8080", &noEnvProvider{})
		require.ErrorContains(t, err, "sign in Docker Desktop")
	})
}

func TestApplyModelOverrides(t *testing.T) {
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
					{Name: "root", Model: "openai/gpt-4o", SubAgents: []string{"agentcatalog/pirate"}},
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
					{Name: "root", Model: "openai/gpt-4o", SubAgents: []string{"helper", "agentcatalog/pirate"}},
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
					{Name: "root", Model: "openai/gpt-4o", Handoffs: []string{"agentcatalog/pirate"}},
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
					{Name: "root", Model: "openai/gpt-4o", SubAgents: []string{"reviewer:agentcatalog/review-pr"}},
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
					{Name: "root", Model: "openai/gpt-4o", Handoffs: []string{"reviewer:agentcatalog/review-pr"}},
				},
			},
		},
		{
			name: "external sub-agent name collides with local agent",
			cfg: &latest.Config{
				Agents: []latest.AgentConfig{
					{Name: "root", Model: "openai/gpt-4o", SubAgents: []string{"pirate", "agentcatalog/pirate"}},
					{Name: "pirate", Model: "openai/gpt-4o"},
				},
			},
			wantErr: "conflicts with a locally-defined agent",
		},
		{
			name: "named external sub-agent collides with local agent",
			cfg: &latest.Config{
				Agents: []latest.AgentConfig{
					{Name: "root", Model: "openai/gpt-4o", SubAgents: []string{"helper", "helper:agentcatalog/review-pr"}},
					{Name: "helper", Model: "openai/gpt-4o"},
				},
			},
			wantErr: "conflicts with a locally-defined agent",
		},
		{
			name: "external handoff name collides with local agent",
			cfg: &latest.Config{
				Agents: []latest.AgentConfig{
					{Name: "root", Model: "openai/gpt-4o", Handoffs: []string{"agentcatalog/pirate"}},
					{Name: "pirate", Model: "openai/gpt-4o"},
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
