package provider

import (
	"maps"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLookupAlias(t *testing.T) {
	t.Parallel()

	// Every entry in the table is reachable.
	for name, expected := range Aliases {
		got, ok := LookupAlias(name)
		assert.True(t, ok, "alias %q should be found", name)
		assert.Equal(t, expected, got)
	}

	// Unknown name yields the zero Alias and false.
	got, ok := LookupAlias("does-not-exist")
	assert.False(t, ok)
	assert.Equal(t, Alias{}, got)

	// Lookup is case-sensitive (callers normalise themselves).
	if _, ok := LookupAlias("MISTRAL"); ok {
		t.Errorf("LookupAlias should be case-sensitive")
	}
}

// TestCatalogAliases asserts each self-contained OpenAI-compatible alias
// resolves to its expected configuration and is reported as both a known and a
// catalog provider. New aliases of the same shape only need a row here.
func TestCatalogAliases(t *testing.T) {
	t.Parallel()

	expected := map[string]Alias{
		"openrouter":  {APIType: "openai", BaseURL: "https://openrouter.ai/api/v1", TokenEnvVar: "OPENROUTER_API_KEY"},
		"baseten":     {APIType: "openai", BaseURL: "https://inference.baseten.co/v1", TokenEnvVar: "BASETEN_API_KEY"},
		"ovhcloud":    {APIType: "openai", BaseURL: "https://oai.endpoints.kepler.ai.cloud.ovh.net/v1", TokenEnvVar: "OVH_AI_ENDPOINTS_ACCESS_TOKEN"},
		"groq":        {APIType: "openai", BaseURL: "https://api.groq.com/openai/v1", TokenEnvVar: "GROQ_API_KEY"},
		"deepseek":    {APIType: "openai", BaseURL: "https://api.deepseek.com/v1", TokenEnvVar: "DEEPSEEK_API_KEY"},
		"cerebras":    {APIType: "openai", BaseURL: "https://api.cerebras.ai/v1", TokenEnvVar: "CEREBRAS_API_KEY"},
		"fireworks":   {APIType: "openai", BaseURL: "https://api.fireworks.ai/inference/v1", TokenEnvVar: "FIREWORKS_API_KEY"},
		"together":    {APIType: "openai", BaseURL: "https://api.together.xyz/v1", TokenEnvVar: "TOGETHER_API_KEY"},
		"huggingface": {APIType: "openai", BaseURL: "https://router.huggingface.co/v1", TokenEnvVar: "HF_TOKEN"},
		"moonshot":    {APIType: "openai", BaseURL: "https://api.moonshot.ai/v1", TokenEnvVar: "MOONSHOT_API_KEY"},
		"nvidia":      {APIType: "openai", BaseURL: "https://integrate.api.nvidia.com/v1", TokenEnvVar: "NVIDIA_API_KEY"},
		"vercel":      {APIType: "openai", BaseURL: "https://ai-gateway.vercel.sh/v1", TokenEnvVar: "AI_GATEWAY_API_KEY"},
	}

	for name, want := range expected {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			alias, ok := LookupAlias(name)
			require.True(t, ok)
			assert.Equal(t, want, alias)
			assert.True(t, IsKnownProvider(name))
			assert.True(t, IsCatalogProvider(name))
		})
	}
}

func TestNebiusAliasUsesTokenFactoryEndpoint(t *testing.T) {
	t.Parallel()

	alias, ok := LookupAlias("nebius")
	require.True(t, ok)
	assert.Equal(t, Alias{
		APIType:     "openai",
		BaseURL:     "https://api.tokenfactory.nebius.com/v1",
		TokenEnvVar: "NEBIUS_API_KEY",
	}, alias)
}

// TestCloudflareAliases asserts the two Cloudflare aliases resolve to their
// account/gateway-scoped, env-templated base URLs. Unlike the static aliases
// above, their BaseURL carries ${...} references that are expanded at
// provider-build time (see cloudflare_alias_test.go for the resolution path).
func TestCloudflareAliases(t *testing.T) {
	t.Parallel()

	expected := map[string]Alias{
		"cloudflare-workers-ai": {
			APIType:     "openai",
			BaseURL:     "https://api.cloudflare.com/client/v4/accounts/${CLOUDFLARE_ACCOUNT_ID}/ai/v1",
			TokenEnvVar: "CLOUDFLARE_API_TOKEN",
		},
		"cloudflare-ai-gateway": {
			APIType:     "openai",
			BaseURL:     "https://gateway.ai.cloudflare.com/v1/${CLOUDFLARE_ACCOUNT_ID}/${CLOUDFLARE_GATEWAY_ID}/compat",
			TokenEnvVar: "CLOUDFLARE_API_TOKEN",
		},
	}

	for name, want := range expected {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			alias, ok := LookupAlias(name)
			require.True(t, ok)
			assert.Equal(t, want, alias)
			assert.True(t, IsKnownProvider(name))
		})
	}
}

func TestEachAlias(t *testing.T) {
	t.Parallel()

	// Iterator yields every entry exactly once.
	collected := maps.Collect(EachAlias())
	assert.Equal(t, Aliases, collected)
}

func TestEachAlias_EarlyTermination(t *testing.T) {
	t.Parallel()

	// Iterator must respect a false return from the yield function.
	require.NotEmpty(t, Aliases, "test requires the alias table to be non-empty")

	count := 0
	for range EachAlias() {
		count++
		if count == 1 {
			break
		}
	}
	assert.Equal(t, 1, count, "iteration should stop when consumer breaks out")
}
