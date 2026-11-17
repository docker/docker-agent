package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/config/legacy"
)

func TestContextWindowValidation(t *testing.T) {
	t.Parallel()

	size := func(n int64) *int64 { return &n }
	openResponses := latest.ProviderConfig{APIType: "openai_responses", BaseURL: "https://gateway.example.com/v1"}
	tests := []struct {
		name      string
		model     latest.ModelConfig
		providers map[string]latest.ProviderConfig
		wantErr   string
	}{
		{
			name:  "model-level value on a non-DMR model is accepted",
			model: latest.ModelConfig{Provider: "openai", Model: "gpt-5", ContextWindow: size(200_000)},
		},
		{
			name:    "zero is rejected",
			model:   latest.ModelConfig{Provider: "openai", Model: "gpt-5", ContextWindow: size(0)},
			wantErr: "models.m: context_window must be greater than 0, got 0",
		},
		{
			name:    "negative is rejected",
			model:   latest.ModelConfig{Provider: "openai", Model: "gpt-5", ContextWindow: size(-1)},
			wantErr: "models.m: context_window must be greater than 0, got -1",
		},
		{
			name:    "provider_opts.context_size on a non-DMR model is rejected with a pointer to context_window",
			model:   latest.ModelConfig{Provider: "openai", Model: "gpt-5", ProviderOpts: map[string]any{"context_size": 8192}},
			wantErr: `models.m: provider_opts.context_size is only used by the dmr provider, not "openai"; move it to context_window on this model`,
		},
		{
			name: "provider_opts.context_size on a custom openai_responses alias is rejected by resolved type",
			model: latest.ModelConfig{
				Provider: "gateway", Model: "my-model", ProviderOpts: map[string]any{"context_size": 8192},
			},
			providers: map[string]latest.ProviderConfig{"gateway": openResponses},
			wantErr:   `provider_opts.context_size is only used by the dmr provider, not "openai_responses"`,
		},
		{
			name: "provider_opts.context_size inherited from a non-DMR custom provider is rejected",
			model: latest.ModelConfig{
				Provider: "gateway", Model: "my-model",
			},
			providers: map[string]latest.ProviderConfig{"gateway": {
				APIType:      "openai_responses",
				ProviderOpts: map[string]any{"context_size": 8192},
			}},
			wantErr: `providers.gateway.provider_opts.context_size is only used by the dmr provider, not "openai_responses"; remove it from the provider and set context_window on models using this provider instead (replace inline provider/model references with named models entries)`,
		},
		{
			name: "a custom alias backed by dmr may keep provider_opts.context_size",
			model: latest.ModelConfig{
				Provider: "local", Model: "ai/qwen3", ProviderOpts: map[string]any{"context_size": 8192},
			},
			providers: map[string]latest.ProviderConfig{"local": {Provider: "dmr"}},
		},
		{
			name: "a custom alias backed by dmr with a model-level context_window that matches",
			model: latest.ModelConfig{
				Provider: "local", Model: "ai/qwen3", ContextWindow: size(8192),
				ProviderOpts: map[string]any{"context_size": 8192},
			},
			providers: map[string]latest.ProviderConfig{"local": {Provider: "dmr"}},
		},
		{
			name: "dmr model-level context_window equal to provider_opts is accepted",
			model: latest.ModelConfig{
				Provider: "dmr", Model: "ai/qwen3", ContextWindow: size(8192),
				ProviderOpts: map[string]any{"context_size": 8192},
			},
		},
		{
			name: "dmr model-level context_window conflicting with provider_opts is rejected",
			model: latest.ModelConfig{
				Provider: "dmr", Model: "ai/qwen3", ContextWindow: size(8192),
				ProviderOpts: map[string]any{"context_size": 4096},
			},
			wantErr: "context_window 8192 conflicts with provider_opts.context_size 4096",
		},
		{
			name: "dmr conflict with provider_opts inherited from a custom provider is rejected",
			model: latest.ModelConfig{
				Provider: "local", Model: "ai/qwen3", ContextWindow: size(8192),
			},
			providers: map[string]latest.ProviderConfig{"local": {
				Provider: "dmr", ProviderOpts: map[string]any{"context_size": 4096},
			}},
			wantErr: "context_window 8192 conflicts with providers.local.provider_opts.context_size 4096",
		},
		{
			name: "model's own provider_opts override the inherited one for DMR",
			model: latest.ModelConfig{
				Provider: "local", Model: "ai/qwen3", ContextWindow: size(8192),
				ProviderOpts: map[string]any{"context_size": 8192},
			},
			providers: map[string]latest.ProviderConfig{"local": {
				Provider: "dmr", ProviderOpts: map[string]any{"context_size": 4096},
			}},
		},
		{
			name:      "inherited api_type overrides underlying dmr and top-level api_type",
			model:     latest.ModelConfig{Provider: "local", Model: "custom"},
			providers: map[string]latest.ProviderConfig{"local": {Provider: "dmr", APIType: "dmr", ProviderOpts: map[string]any{"api_type": "openai_chatcompletions", "context_size": 8192}}},
			wantErr:   `not "openai_chatcompletions"`,
		},
		{
			name:      "model api_type overrides inherited api_type",
			model:     latest.ModelConfig{Provider: "local", Model: "custom", ProviderOpts: map[string]any{"api_type": "dmr"}},
			providers: map[string]latest.ProviderConfig{"local": {Provider: "dmr", ProviderOpts: map[string]any{"api_type": "openai_chatcompletions", "context_size": 8192}}},
		},
		{
			name:      "present empty model api_type suppresses inherited api_type",
			model:     latest.ModelConfig{Provider: "local", Model: "custom", ProviderOpts: map[string]any{"api_type": ""}},
			providers: map[string]latest.ProviderConfig{"local": {Provider: "dmr", APIType: "openai_responses", ProviderOpts: map[string]any{"api_type": "openai_chatcompletions", "context_size": 8192}}},
		},

		{
			name: "first_available cannot combine with context_window",
			model: latest.ModelConfig{
				FirstAvailable: []string{"a", "b"}, ContextWindow: size(8192),
			},
			wantErr: "first_available cannot be combined with context_window",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := &latest.Config{Models: map[string]latest.ModelConfig{"m": tt.model}, Providers: tt.providers}
			err := cfg.Validate()
			if err == nil {
				err = ValidateContextWindows(cfg)
			}
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestContextWindowMigrationRequiresCurrentVersion(t *testing.T) {
	t.Parallel()
	const model = `
models:
  custom:
    provider: openai
    model: custom
    context_window: 8192
agents:
  root:
    model: custom
    instruction: test
`
	_, err := Load(t.Context(), NewBytesSource("old.yaml", []byte("version: \"15\"\n"+model)), WithVersions(legacy.Register))
	require.ErrorContains(t, err, "context_window")
	cfg, err := Load(t.Context(), NewBytesSource("migrated.yaml", []byte("version: \"16\"\n"+model)), WithVersions(legacy.Register))
	require.NoError(t, err)
	custom := cfg.Models["custom"]
	assert.Equal(t, int64(8192), custom.DeclaredContextWindow())
}
