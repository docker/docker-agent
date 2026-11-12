package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

// capStore supplies a fixed catalogue without network access.
func capStore(t *testing.T) *modelsdev.Store {
	t.Helper()
	return modelsdev.NewDatabaseStore(&modelsdev.Database{Providers: map[string]modelsdev.Provider{
		"anthropic": {Models: map[string]modelsdev.Model{
			"claude": {Limit: modelsdev.Limit{Context: 200_000}},
		}},
		"google-vertex": {Models: map[string]modelsdev.Model{
			"meta/llama": {Limit: modelsdev.Limit{Context: 128_000}},
		}},
		"openai": {Models: map[string]modelsdev.Model{
			"gpt-4o":      {Limit: modelsdev.Limit{Context: 128_000}},
			"gpt-4o-mini": {Limit: modelsdev.Limit{Context: 128_000}},
		}},
	}})
}

func TestValidateContextWindowCaps(t *testing.T) {
	t.Parallel()

	size := func(n int64) *int64 { return &n }
	tests := []struct {
		name      string
		models    map[string]latest.ModelConfig
		providers map[string]latest.ProviderConfig
		wantErr   string
	}{
		{
			name:    "Vertex Anthropic uses runtime publisher identity during preflight",
			models:  map[string]latest.ModelConfig{"m": {Provider: "google", Model: "claude", ContextWindow: size(300_000), ProviderOpts: map[string]any{"publisher": "anthropic"}}},
			wantErr: "exceeds the context window of anthropic/claude, which is 200000 tokens",
		},
		{
			name:      "Vertex inherited publisher keeps non-Anthropic model prefix during preflight",
			models:    map[string]latest.ModelConfig{"m": {Provider: "custom", Model: "meta/llama", ContextWindow: size(200_000)}},
			providers: map[string]latest.ProviderConfig{"custom": {Provider: "google", ProviderOpts: map[string]any{"publisher": "meta"}}},
			wantErr:   "exceeds the context window of google-vertex/meta/llama, which is 128000 tokens",
		},
		{
			name:   "endpoint variables are not needed for catalogue lookup",
			models: map[string]latest.ModelConfig{"m": {Provider: "openai", Model: "gpt-4o", BaseURL: "${REMOTE_URL}", ContextWindow: size(128_000)}},
		},
		{
			name:    "missing endpoint does not bypass a known cap",
			models:  map[string]latest.ModelConfig{"m": {Provider: "openai", Model: "gpt-4o", BaseURL: "${REMOTE_URL}", ContextWindow: size(200_000)}},
			wantErr: "exceeds the context window of openai/gpt-4o",
		},
		{
			name:   "at the catalogue max is allowed",
			models: map[string]latest.ModelConfig{"m": {Provider: "openai", Model: "gpt-4o", ContextWindow: size(128_000)}},
		},
		{
			name:   "below the catalogue max is allowed",
			models: map[string]latest.ModelConfig{"m": {Provider: "openai", Model: "gpt-4o", ContextWindow: size(32_000)}},
		},
		{
			name:    "above the catalogue max is rejected naming the model, field and max",
			models:  map[string]latest.ModelConfig{"big": {Provider: "openai", Model: "gpt-4o", ContextWindow: size(200_000)}},
			wantErr: "models.big: context_window 200000 exceeds the context window of openai/gpt-4o, which is 128000 tokens in the models.dev catalogue; set context_window to at most 128000",
		},
		{
			name:   "a model the catalogue does not describe accepts any positive value",
			models: map[string]latest.ModelConfig{"m": {Provider: "openai", Model: "not-in-catalogue", ContextWindow: size(10_000_000)}},
		},
		{
			name:   "an openai_responses gateway with no catalogue identity accepts any positive value",
			models: map[string]latest.ModelConfig{"m": {Provider: "gateway", Model: "gpt-4o", ContextWindow: size(10_000_000)}},
			providers: map[string]latest.ProviderConfig{"gateway": {
				APIType: "openai_responses", BaseURL: "https://gateway.example.com/v1",
			}},
		},
		{
			name:   "a custom provider backed by openai is checked against openai's catalogue",
			models: map[string]latest.ModelConfig{"m": {Provider: "proxy", Model: "gpt-4o", ContextWindow: size(200_000)}},
			providers: map[string]latest.ProviderConfig{"proxy": {
				Provider: "openai", BaseURL: "https://proxy.example.com/v1",
			}},
			wantErr: "models.m: context_window 200000 exceeds the context window of openai/gpt-4o",
		},
		{
			name: "a dedicated compaction model entry is checked like any other model",
			models: map[string]latest.ModelConfig{
				"primary":    {Provider: "openai", Model: "gpt-4o", CompactionModel: "summarizer"},
				"summarizer": {Provider: "openai", Model: "gpt-4o-mini", ContextWindow: size(200_000)},
			},
			wantErr: "models.summarizer: context_window 200000 exceeds the context window of openai/gpt-4o-mini",
		},
		{
			name:    "the raw catalogue limit is compared, so a larger declared value cannot hide the conflict",
			models:  map[string]latest.ModelConfig{"m": {Provider: "openai", Model: "gpt-4o", ContextWindow: size(129_000)}},
			wantErr: "exceeds the context window of openai/gpt-4o, which is 128000 tokens",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := &latest.Config{Models: tt.models, Providers: tt.providers}
			err := ValidateContextWindowCaps(t.Context(), cfg, capStore(t), environment.NewNoEnvProvider())
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestValidateContextWindowCapsWithoutCatalogue(t *testing.T) {
	t.Parallel()

	size := int64(8192)
	cfg := &latest.Config{Models: map[string]latest.ModelConfig{"m": {Provider: "openai", Model: "gpt-4o", ContextWindow: &size}}}
	err := ValidateContextWindowCaps(t.Context(), cfg, nil, environment.NewNoEnvProvider())
	require.Error(t, err, "a declared window must not be accepted unchecked when the catalogue is unavailable")
	assert.Contains(t, err.Error(), "models.m: cannot check context_window")

	cfg.Models["m"] = latest.ModelConfig{Provider: "openai", Model: "${MISSING}", ContextWindow: &size}
	require.ErrorContains(t, ValidateContextWindowCaps(t.Context(), cfg, nil, environment.NewNoEnvProvider()), "models.m: cannot check context_window", "environment-dependent identities still require a catalogue")

	none := &latest.Config{Models: map[string]latest.ModelConfig{"m": {Provider: "openai", Model: "gpt-4o"}}}
	require.NoError(t, ValidateContextWindowCaps(t.Context(), none, nil, environment.NewNoEnvProvider()), "no declared window needs no catalogue")
}

func TestValidateContextWindowCapsDefersModelEnvironment(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, model, display string
		window               int64
	}{
		{name: "bare shell syntax", model: "$MODEL", window: 200_000},
		{name: "shell syntax over cap", model: "${MODEL}", window: 200_000},
		{name: "template syntax over cap", model: "${env.MODEL}", window: 200_000},
		{name: "display identity", model: "resolved-id", display: "${MODEL}", window: 200_000},
		{name: "at cap", model: "${MODEL}", window: 128_000},
		{name: "unknown model", model: "${UNKNOWN}", window: 200_000},
		{name: "missing variable", model: "${MISSING}", window: 200_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &latest.Config{Models: map[string]latest.ModelConfig{"m": {
				Provider: "openai", Model: tc.model, DisplayModel: tc.display, ContextWindow: &tc.window,
			}}}
			env := environment.NewMapEnvProvider(map[string]string{"MODEL": "gpt-4o", "UNKNOWN": "not-in-catalogue"})
			err := ValidateContextWindowCaps(t.Context(), cfg, capStore(t), env)
			require.NoError(t, err, "environment-dependent identities are validated only when constructed")
			assert.Equal(t, tc.model, cfg.Models["m"].Model, "validation must not mutate the model")
		})
	}
}

func TestValidateContextWindowCapsStaticDisplayIdentity(t *testing.T) {
	t.Parallel()
	window := int64(200_000)
	cfg := &latest.Config{Models: map[string]latest.ModelConfig{"m": {
		Provider: "openai", Model: "${MISSING}", DisplayModel: "gpt-4o", ContextWindow: &window,
	}}}
	err := ValidateContextWindowCaps(t.Context(), cfg, capStore(t), environment.NewNoEnvProvider())
	require.ErrorContains(t, err, "exceeds the context window of openai/gpt-4o", "a static catalogue identity must still be checked even if the runtime model contains environment refs")
}
