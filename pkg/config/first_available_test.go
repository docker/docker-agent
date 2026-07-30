package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

func TestResolveFirstAvailableModels_SelectsFirstWithCredentials(t *testing.T) {
	t.Parallel()

	cfg := &latest.Config{
		Models: map[string]latest.ModelConfig{
			"smart": {
				FirstAvailable: []string{
					"anthropic/claude-sonnet-4-6",
					"openai/gpt-5",
					"dmr/ai/qwen3",
				},
			},
		},
	}

	env := environment.NewMapEnvProvider(map[string]string{
		"OPENAI_API_KEY": "test-key",
	})

	require.NoError(t, ResolveFirstAvailableModels(t.Context(), cfg, "", env))

	got := cfg.Models["smart"]
	assert.Equal(t, "openai", got.Provider)
	assert.Equal(t, "gpt-5", got.Model)
	assert.Empty(t, got.FirstAvailable)
}

func TestResolveFirstAvailableModels_PreservesSelectorDescription(t *testing.T) {
	t.Parallel()

	cfg := &latest.Config{
		Models: map[string]latest.ModelConfig{
			"smart": {
				Description: "Best available reasoning model.",
				FirstAvailable: []string{
					"openai/gpt-5",
					"dmr/ai/qwen3",
				},
			},
		},
	}

	env := environment.NewMapEnvProvider(map[string]string{
		"OPENAI_API_KEY": "test-key",
	})

	require.NoError(t, ResolveFirstAvailableModels(t.Context(), cfg, "", env))

	got := cfg.Models["smart"]
	assert.Equal(t, "openai", got.Provider)
	assert.Equal(t, "Best available reasoning model.", got.Description)
}

func TestResolveFirstAvailableModels_FallsBackToLocalProvider(t *testing.T) {
	t.Parallel()

	cfg := &latest.Config{
		Models: map[string]latest.ModelConfig{
			"smart": {
				FirstAvailable: []string{
					"anthropic/claude-sonnet-4-6",
					"openai/gpt-5",
					"dmr/ai/qwen3",
				},
			},
		},
	}

	// No API keys: dmr needs none, so it is selected.
	require.NoError(t, ResolveFirstAvailableModels(t.Context(), cfg, "", environment.NewNoEnvProvider()))

	got := cfg.Models["smart"]
	assert.Equal(t, "dmr", got.Provider)
	assert.Equal(t, "ai/qwen3", got.Model)
}

func TestResolveFirstAvailableModels_NamedCandidate(t *testing.T) {
	t.Parallel()

	cfg := &latest.Config{
		Models: map[string]latest.ModelConfig{
			"claude": {Provider: "anthropic", Model: "claude-sonnet-4-6"},
			"gpt":    {Provider: "openai", Model: "gpt-5"},
			"smart":  {FirstAvailable: []string{"claude", "gpt"}},
		},
	}

	env := environment.NewMapEnvProvider(map[string]string{
		"ANTHROPIC_API_KEY": "test-key",
	})

	require.NoError(t, ResolveFirstAvailableModels(t.Context(), cfg, "", env))

	got := cfg.Models["smart"]
	assert.Equal(t, "anthropic", got.Provider)
	assert.Equal(t, "claude-sonnet-4-6", got.Model)
}

// TestResolveFirstAvailableModels_PreservesConcreteCandidateOutputCapabilities is
// the regression for a selected candidate's owner-declared
// output_capabilities.image: resolveCandidate returns a named model's
// ModelConfig as-is (see resolveCandidate in first_available.go), but a
// future change to that path (e.g. rebuilding the struct field-by-field, or
// routing through ParseModelRef) could silently drop it. A selector itself
// can never carry output_capabilities (see TestValidateFirstAvailable's
// "combined with output_capabilities" cases) — only a concrete candidate can
// — so this is the only place that guarantee can be pinned end to end.
func TestResolveFirstAvailableModels_PreservesConcreteCandidateOutputCapabilities(t *testing.T) {
	t.Parallel()

	cfg := &latest.Config{
		Models: map[string]latest.ModelConfig{
			"gemini_image": {
				Provider:           "google",
				Model:              "gemini-2.5-flash-image",
				OutputCapabilities: &latest.OutputCapabilitiesConfig{Image: new(true)},
			},
			"smart": {FirstAvailable: []string{"gemini_image", "dmr/ai/qwen3"}},
		},
	}

	env := environment.NewMapEnvProvider(map[string]string{"GEMINI_API_KEY": "test-key"})

	require.NoError(t, ResolveFirstAvailableModels(t.Context(), cfg, "", env))

	got := cfg.Models["smart"]
	assert.Equal(t, "google", got.Provider)
	assert.Equal(t, "gemini-2.5-flash-image", got.Model)
	require.NotNil(t, got.OutputCapabilities,
		"the selected concrete candidate's output_capabilities block must survive selection/resolution")
	require.NotNil(t, got.OutputCapabilities.Image)
	assert.True(t, *got.OutputCapabilities.Image)
}

func TestResolveFirstAvailableModels_SkipsRoutingCandidateWithMissingCredentials(t *testing.T) {
	t.Parallel()

	cfg := &latest.Config{
		Models: map[string]latest.ModelConfig{
			"router": {
				Provider: "openai",
				Model:    "gpt-5",
				Routing: []latest.RoutingRule{
					{Model: "anthropic/claude-sonnet-4-6", Examples: []string{"reasoning"}},
				},
			},
			"smart": {FirstAvailable: []string{"router", "dmr/ai/qwen3"}},
		},
	}

	env := environment.NewMapEnvProvider(map[string]string{
		"OPENAI_API_KEY": "test-key",
	})

	require.NoError(t, ResolveFirstAvailableModels(t.Context(), cfg, "", env))

	got := cfg.Models["smart"]
	assert.Equal(t, "dmr", got.Provider)
	assert.Equal(t, "ai/qwen3", got.Model)
}

func TestResolveFirstAvailableModels_RejectsRoutingCandidateWithNestedRouter(t *testing.T) {
	t.Parallel()

	cfg := &latest.Config{
		Models: map[string]latest.ModelConfig{
			"inner_router": {
				Provider: "openai",
				Model:    "gpt-5",
				Routing: []latest.RoutingRule{
					{Model: "anthropic/claude-sonnet-4-6", Examples: []string{"reasoning"}},
				},
			},
			"outer_router": {
				Provider: "openai",
				Model:    "gpt-5",
				Routing: []latest.RoutingRule{
					{Model: "inner_router", Examples: []string{"reasoning"}},
				},
			},
			"smart": {FirstAvailable: []string{"outer_router", "dmr/ai/qwen3"}},
		},
	}

	err := ResolveFirstAvailableModels(t.Context(), cfg, "", environment.NewMapEnvProvider(map[string]string{
		"OPENAI_API_KEY": "test-key",
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot be used as a first_available candidate dependency")
}

func TestResolveFirstAvailableModels_GatewaySelectsFirst(t *testing.T) {
	t.Parallel()

	cfg := &latest.Config{
		Models: map[string]latest.ModelConfig{
			"smart": {
				FirstAvailable: []string{
					"anthropic/claude-sonnet-4-6",
					"openai/gpt-5",
				},
			},
		},
	}

	require.NoError(t, ResolveFirstAvailableModels(t.Context(), cfg, "gateway:8080", environment.NewNoEnvProvider()))

	got := cfg.Models["smart"]
	assert.Equal(t, "anthropic", got.Provider)
	assert.Equal(t, "claude-sonnet-4-6", got.Model)
}

func TestResolveFirstAvailableModels_GatewaySkipsBypassingCandidateWithMissingCredentials(t *testing.T) {
	t.Parallel()

	cfg := &latest.Config{
		Providers: map[string]latest.ProviderConfig{
			"my_endpoint": {BaseURL: "https://llm.internal.example.com/v1", TokenKey: "MY_TOKEN"},
		},
		Models: map[string]latest.ModelConfig{
			"direct": {Provider: "anthropic", Model: "claude-sonnet-4-6", BypassModelsGateway: true},
			"smart": {
				FirstAvailable: []string{
					"my_endpoint/llama-3", // custom base_url implies the bypass; MY_TOKEN unset
					"direct",              // explicit bypass; ANTHROPIC_API_KEY unset
					"openai/gpt-5",        // gateway supplies credentials
				},
			},
		},
	}

	// Bypassing candidates dial their endpoint directly even under a gateway,
	// so they need their own credentials to be selected.
	require.NoError(t, ResolveFirstAvailableModels(t.Context(), cfg, "gateway:8080", environment.NewNoEnvProvider()))

	got := cfg.Models["smart"]
	assert.Equal(t, "openai", got.Provider)
	assert.Equal(t, "gpt-5", got.Model)
}

func TestResolveFirstAvailableModels_GatewaySelectsBypassingCandidateWithCredentials(t *testing.T) {
	t.Parallel()

	cfg := &latest.Config{
		Providers: map[string]latest.ProviderConfig{
			"my_endpoint": {BaseURL: "https://llm.internal.example.com/v1", TokenKey: "MY_TOKEN"},
		},
		Models: map[string]latest.ModelConfig{
			"smart": {
				FirstAvailable: []string{
					"my_endpoint/llama-3",
					"openai/gpt-5",
				},
			},
		},
	}

	env := environment.NewMapEnvProvider(map[string]string{"MY_TOKEN": "test-key"})
	require.NoError(t, ResolveFirstAvailableModels(t.Context(), cfg, "gateway:8080", env))

	got := cfg.Models["smart"]
	assert.Equal(t, "my_endpoint", got.Provider)
	assert.Equal(t, "llama-3", got.Model)
}

func TestResolveFirstAvailableModels_NoCandidateAvailable(t *testing.T) {
	t.Parallel()

	cfg := &latest.Config{
		Models: map[string]latest.ModelConfig{
			"smart": {
				FirstAvailable: []string{
					"anthropic/claude-sonnet-4-6",
					"openai/gpt-5",
				},
			},
		},
	}

	err := ResolveFirstAvailableModels(t.Context(), cfg, "", environment.NewNoEnvProvider())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "No 'first_available' candidate has credentials configured.")
	assert.Contains(t, err.Error(), "Set the environment variables for at least one candidate:")
	assert.Contains(t, err.Error(), " - anthropic/claude-sonnet-4-6: ANTHROPIC_API_KEY")
	assert.Contains(t, err.Error(), " - openai/gpt-5: OPENAI_API_KEY")
	assert.Contains(t, err.Error(), "export ANTHROPIC_API_KEY=<value>")
	assert.Contains(t, err.Error(), "--env-from-file")
	assert.Contains(t, err.Error(), environment.SecretsDocsURL)
	assert.Contains(t, err.Error(), environment.ModelSetupDocsURL)
}

func TestResolveFirstAvailableModels_InvalidCandidate(t *testing.T) {
	t.Parallel()

	cfg := &latest.Config{
		Models: map[string]latest.ModelConfig{
			"smart": {FirstAvailable: []string{"not-a-valid-ref"}},
		},
	}

	err := ResolveFirstAvailableModels(t.Context(), cfg, "", environment.NewNoEnvProvider())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a known model")
}

func TestResolveFirstAvailableModels_UnknownProvider(t *testing.T) {
	t.Parallel()

	cfg := &latest.Config{
		Models: map[string]latest.ModelConfig{
			"smart": {FirstAvailable: []string{"unknown/model", "dmr/ai/qwen3"}},
		},
	}

	err := ResolveFirstAvailableModels(t.Context(), cfg, "", environment.NewNoEnvProvider())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "uses unknown provider 'unknown'")
}

func TestResolveFirstAvailableModels_RejectsNestedSelector(t *testing.T) {
	t.Parallel()

	cfg := &latest.Config{
		Models: map[string]latest.ModelConfig{
			"inner": {FirstAvailable: []string{"dmr/ai/qwen3"}},
			"smart": {FirstAvailable: []string{"inner"}},
		},
	}

	err := ResolveFirstAvailableModels(t.Context(), cfg, "", environment.NewNoEnvProvider())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "itself a first_available selector")
}

func TestValidateFirstAvailable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		model   latest.ModelConfig
		wantErr string
	}{
		{
			name:  "valid",
			model: latest.ModelConfig{FirstAvailable: []string{"openai/gpt-5"}},
		},
		{
			name:    "empty list",
			model:   latest.ModelConfig{FirstAvailable: []string{}},
			wantErr: "must contain at least one candidate",
		},
		{
			name:    "combined with provider/model",
			model:   latest.ModelConfig{Provider: "openai", Model: "gpt-5", FirstAvailable: []string{"openai/gpt-5"}},
			wantErr: "cannot be combined with provider/model",
		},
		{
			name: "combined with routing",
			model: latest.ModelConfig{
				FirstAvailable: []string{"openai/gpt-5"},
				Routing:        []latest.RoutingRule{{Model: "openai/gpt-5", Examples: []string{"x"}}},
			},
			wantErr: "cannot be combined with routing",
		},
		{
			name:    "combined with title_model",
			model:   latest.ModelConfig{FirstAvailable: []string{"openai/gpt-5"}, TitleModel: "openai/gpt-4o-mini"},
			wantErr: "cannot be combined with title_model",
		},
		{
			name:    "combined with compaction_model",
			model:   latest.ModelConfig{FirstAvailable: []string{"openai/gpt-5"}, CompactionModel: "openai/gpt-4o-mini"},
			wantErr: "cannot be combined with compaction_model",
		},
		{
			name:    "combined with compaction_threshold",
			model:   latest.ModelConfig{FirstAvailable: []string{"openai/gpt-5"}, CompactionThreshold: new(0.8)},
			wantErr: "cannot be combined with compaction_threshold",
		},
		{
			name:    "combined with token key",
			model:   latest.ModelConfig{FirstAvailable: []string{"openai/gpt-5"}, TokenKey: "CUSTOM_API_KEY"},
			wantErr: "cannot be combined with token_key",
		},
		{
			name:    "combined with bypass_models_gateway",
			model:   latest.ModelConfig{FirstAvailable: []string{"openai/gpt-5"}, BypassModelsGateway: true},
			wantErr: "cannot be combined with bypass_models_gateway",
		},
		{
			name:    "combined with model options",
			model:   latest.ModelConfig{FirstAvailable: []string{"openai/gpt-5"}, ProviderOpts: map[string]any{"project": "p"}},
			wantErr: "cannot be combined with provider_opts",
		},
		{
			name:    "combined with auth",
			model:   latest.ModelConfig{FirstAvailable: []string{"anthropic/claude-sonnet-4-6"}, Auth: &latest.AuthConfig{Type: "anthropic_wif"}},
			wantErr: "cannot be combined with auth",
		},
		{
			name:    "combined with output_capabilities",
			model:   latest.ModelConfig{FirstAvailable: []string{"google/gemini-2.5-flash-image"}, OutputCapabilities: &latest.OutputCapabilitiesConfig{Image: new(true)}},
			wantErr: "cannot be combined with output_capabilities",
		},
		{
			name:    "combined with output_capabilities false",
			model:   latest.ModelConfig{FirstAvailable: []string{"google/gemini-2.5-flash-image"}, OutputCapabilities: &latest.OutputCapabilitiesConfig{Image: new(false)}},
			wantErr: "cannot be combined with output_capabilities",
		},
		{
			name:    "empty candidate",
			model:   latest.ModelConfig{FirstAvailable: []string{"  "}},
			wantErr: "must not be empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := latest.Config{Models: map[string]latest.ModelConfig{"smart": tt.model}}
			err := cfg.Validate()
			if tt.wantErr == "" {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			}
		})
	}
}

func TestResolveFirstAvailableModels_IgnoresUnusedUnavailableSelector(t *testing.T) {
	t.Parallel()

	cfg := &latest.Config{
		Agents: []latest.AgentConfig{{Name: "agent", Model: "used"}},
		Models: map[string]latest.ModelConfig{
			"used":   {FirstAvailable: []string{"dmr/ai/qwen3"}},
			"unused": {FirstAvailable: []string{"anthropic/claude-sonnet-4-6"}},
		},
	}

	require.NoError(t, ResolveFirstAvailableModels(t.Context(), cfg, "", environment.NewNoEnvProvider()))

	assert.Equal(t, "dmr", cfg.Models["used"].Provider)
	unused := cfg.Models["unused"]
	assert.True(t, unused.IsFirstAvailable())
}

func TestResolveFirstAvailableModels_GeminiCredentialsFromEnvProvider(t *testing.T) {
	t.Parallel()

	cfg := &latest.Config{
		Agents: []latest.AgentConfig{{Name: "agent", Model: "smart"}},
		Models: map[string]latest.ModelConfig{
			"smart": {FirstAvailable: []string{"google/gemini-2.5-pro", "dmr/ai/qwen3"}},
		},
	}
	env := environment.NewMapEnvProvider(map[string]string{"GEMINI_API_KEY": "test-key"})

	require.NoError(t, ResolveFirstAvailableModels(t.Context(), cfg, "", env))

	got := cfg.Models["smart"]
	assert.Equal(t, "google", got.Provider)
	assert.Equal(t, "gemini-2.5-pro", got.Model)
}

func TestResolveFirstAvailableModels_ProcessEnvDoesNotAffectGeminiSelection(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "process-key")

	cfg := &latest.Config{
		Agents: []latest.AgentConfig{{Name: "agent", Model: "smart"}},
		Models: map[string]latest.ModelConfig{
			"smart": {FirstAvailable: []string{"google/gemini-2.5-pro", "dmr/ai/qwen3"}},
		},
	}

	require.NoError(t, ResolveFirstAvailableModels(t.Context(), cfg, "", environment.NewNoEnvProvider()))

	got := cfg.Models["smart"]
	assert.Equal(t, "dmr", got.Provider)
	assert.Equal(t, "ai/qwen3", got.Model)
}

func TestResolveFirstAvailableModels_RejectsRoutingTargetFirstAvailableSelector(t *testing.T) {
	t.Parallel()

	cfg := &latest.Config{
		Agents: []latest.AgentConfig{{Name: "agent", Model: "smart"}},
		Models: map[string]latest.ModelConfig{
			"nested": {FirstAvailable: []string{"anthropic/claude-sonnet-4-6"}},
			"router": {
				Provider: "openai",
				Model:    "gpt-5",
				Routing: []latest.RoutingRule{
					{Model: "nested", Examples: []string{"reasoning"}},
				},
			},
			"smart": {FirstAvailable: []string{"router", "dmr/ai/qwen3"}},
		},
	}

	err := ResolveFirstAvailableModels(t.Context(), cfg, "", environment.NewMapEnvProvider(map[string]string{
		"OPENAI_API_KEY": "test-key",
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "routing target 'nested' is a first_available selector")
}

func TestResolveFirstAvailableModels_SelectsUnauthenticatedCustomProvider(t *testing.T) {
	t.Parallel()

	cfg := &latest.Config{
		Agents: []latest.AgentConfig{{Name: "agent", Model: "smart"}},
		Providers: map[string]latest.ProviderConfig{
			"local_openai": {BaseURL: "http://localhost:1234/v1"},
		},
		Models: map[string]latest.ModelConfig{
			"smart": {FirstAvailable: []string{"local_openai/qwen3", "dmr/ai/qwen3"}},
		},
	}

	require.NoError(t, ResolveFirstAvailableModels(t.Context(), cfg, "", environment.NewNoEnvProvider()))

	got := cfg.Models["smart"]
	assert.Equal(t, "local_openai", got.Provider)
	assert.Equal(t, "qwen3", got.Model)
}
