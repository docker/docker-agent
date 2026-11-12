package provider

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

func TestCanonicalProviderDefaults(t *testing.T) {
	t.Parallel()
	for _, legacy := range []string{"fireworks", "together", "moonshot", "opencode-zen"} {
		t.Run(legacy, func(t *testing.T) {
			t.Parallel()
			canonical := modelsdev.CanonicalProviderID(legacy)
			cfg := &latest.ModelConfig{Provider: legacy, Model: "Mixed/Model"}
			got := applyProviderDefaults(cfg, nil)
			want := applyProviderDefaults(&latest.ModelConfig{Provider: canonical, Model: cfg.Model}, nil)
			assert.Equal(t, want, got)
			assert.Equal(t, legacy, cfg.Provider)
			assert.Equal(t, canonical, got.Provider)
			assert.Equal(t, cfg.Model, got.Model)
			assert.Equal(t, "openai", resolveProviderType(got), "transport is not a naming alias")

			custom := latest.ProviderConfig{BaseURL: "https://custom.invalid/v1", TokenKey: "CUSTOM_TOKEN", APIType: "openai_chatcompletions"}
			providers := map[string]latest.ProviderConfig{legacy: custom, canonical: {BaseURL: "https://canonical-custom.invalid/v1", APIType: "openai_chatcompletions"}}
			got = applyProviderDefaults(cfg, providers)
			assert.Equal(t, legacy, got.Provider, "custom key wins before normalization")
			assert.Equal(t, custom.BaseURL, got.BaseURL)
			assert.Equal(t, custom.TokenKey, got.TokenKey)
			assert.Equal(t, "openai_chatcompletions", resolveProviderType(got))
			assert.Equal(t, providers[canonical].BaseURL, applyProviderDefaults(&latest.ModelConfig{Provider: canonical, Model: cfg.Model}, providers).BaseURL)

			providers = map[string]latest.ProviderConfig{"mine": {Provider: legacy, BaseURL: custom.BaseURL, TokenKey: custom.TokenKey}}
			got = applyProviderDefaults(&latest.ModelConfig{Provider: "mine", Model: cfg.Model}, providers)
			assert.Equal(t, canonical, got.Provider)
			assert.Equal(t, custom.BaseURL, got.BaseURL)
			assert.Equal(t, custom.TokenKey, got.TokenKey)

			cfg.BaseURL, cfg.TokenKey = "https://override.invalid/v1", "OVERRIDE_TOKEN"
			got = applyProviderDefaults(cfg, nil)
			require.Equal(t, cfg.BaseURL, got.BaseURL)
			assert.Equal(t, cfg.TokenKey, got.TokenKey)
		})
	}
}
