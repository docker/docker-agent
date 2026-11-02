package provider

import (
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chatgpt"
	"github.com/docker/docker-agent/pkg/model/provider/catalog"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

func TestFindProvider(t *testing.T) {
	t.Parallel()

	// Every entry in the table is reachable.
	for name, expected := range catalog.Providers {
		got, ok := catalog.Find(name)
		assert.True(t, ok, "alias %q should be found", name)
		assert.Equal(t, expected, got)
	}

	// Unknown name yields the zero catalog.Provider and false.
	got, ok := catalog.Find("does-not-exist")
	assert.False(t, ok)
	assert.Equal(t, catalog.Provider{}, got)

	// Lookup is case-sensitive (callers normalise themselves).
	if _, ok := catalog.Find("MISTRAL"); ok {
		t.Errorf("catalog.Find should be case-sensitive")
	}
}

func TestProviderConfiguration(t *testing.T) {
	t.Parallel()

	for name, alias := range catalog.Providers {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.NotEmpty(t, name)
			assert.Equal(t, strings.ToLower(name), name)
			assert.Contains(t, []string{"openai", "anthropic", "google", "dmr", "amazon-bedrock"}, alias.APIType)
			for _, key := range alias.TokenEnvVars {
				assert.Equal(t, strings.TrimSpace(key), key)
				assert.NotContains(t, key, " ")
			}
			if alias.BaseURL != "" {
				endpoint, err := url.Parse(strings.ReplaceAll(alias.BaseURL, "{region}", "us-east-1"))
				require.NoError(t, err)
				assert.NotEmpty(t, endpoint.Host)
				assert.True(t, slices.Contains([]string{"http", "https"}, endpoint.Scheme))
			}
		})
	}
}

func TestChatGPTAliasMatchesLogin(t *testing.T) {
	t.Parallel()

	alias, ok := catalog.Find(chatgpt.ProviderName)
	require.True(t, ok)
	assert.Equal(t, chatgpt.BaseURL, alias.BaseURL)
	assert.Equal(t, chatgpt.TokenEnvVar, alias.TokenEnvVars[0])
}

func TestProviderIDsMatchEmbeddedCatalog(t *testing.T) {
	t.Parallel()
	db := modelsdev.EmbeddedSnapshot()
	absent := map[string]string{
		"api-route":  "model IDs come from API Route's /v1/models endpoint, not models.dev",
		"atlascloud": "model IDs come from Atlas Cloud's /v1/models endpoint, not models.dev",
		"cloudgpu":   "model IDs come from CloudGPU's /v1/models endpoint, not models.dev",
		"atmorouter": "model IDs come from AtmoRouter's /v1/models endpoint, not models.dev",
		"chatgpt":    "subscription backend, distinct from OpenAI",
		"lunaroute":  "model IDs come from Lunaroute’s /v1/models endpoint, not models.dev",
		"runcrate":   "model IDs come from Runcrate's /v1/models endpoint, not models.dev",
		"dmr":        "local Docker Model Runner",
		"ollama":     "local Ollama server",
	}
	for _, name := range catalog.AllProviders() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, name, modelsdev.CanonicalProviderID(name))
			_, exists := db.Providers[name]
			if reason, exempt := absent[name]; exempt {
				assert.False(t, exists, "remove stale exemption: %s", reason)
			} else {
				assert.True(t, exists, "built-in provider must use its models.dev ID")
			}
		})
	}
	for _, legacy := range []string{"fireworks", "together", "moonshot", "opencode-zen"} {
		canonical := modelsdev.CanonicalProviderID(legacy)
		assert.NotEqual(t, legacy, canonical)
		assert.NotContains(t, catalog.Providers, legacy)
		assert.NotContains(t, catalog.AllProviders(), legacy)
		assert.NotContains(t, db.Providers, legacy, "legacy ID now collides with an upstream service")
		require.Contains(t, catalog.Providers, canonical)
		got, ok := catalog.Find(legacy)
		require.True(t, ok)
		assert.Equal(t, catalog.Providers[canonical], got)
		assert.True(t, catalog.IsKnownProvider(legacy))
	}
}
