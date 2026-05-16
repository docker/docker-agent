package provider

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCatalogProviders(t *testing.T) {
	t.Parallel()

	providers := CatalogProviders()

	// Should include all core providers
	for _, core := range CoreProviders {
		assert.Contains(t, providers, core, "should include core provider %s", core)
	}

	// Should include aliases with BaseURL
	for name, alias := range EachAlias() {
		if alias.BaseURL != "" {
			assert.Contains(t, providers, name, "should include alias %s with BaseURL", name)
		} else {
			assert.NotContains(t, providers, name, "should NOT include alias %s without BaseURL", name)
		}
	}
}

func TestIsCatalogProvider(t *testing.T) {
	t.Parallel()

	// All core providers should be catalog providers
	for _, core := range CoreProviders {
		assert.True(t, IsCatalogProvider(core), "core provider %s should be a catalog provider", core)
	}

	// Aliases: catalog if and only if they have a BaseURL
	for name, alias := range EachAlias() {
		if alias.BaseURL != "" {
			assert.True(t, IsCatalogProvider(name), "alias %s with BaseURL should be a catalog provider", name)
		} else {
			assert.False(t, IsCatalogProvider(name), "alias %s without BaseURL should NOT be a catalog provider", name)
		}
	}

	// Unknown providers
	assert.False(t, IsCatalogProvider("unknown"))
	assert.False(t, IsCatalogProvider("cohere"))
}

func TestAllProviders(t *testing.T) {
	t.Parallel()

	all := AllProviders()

	// Should include all core providers
	for _, core := range CoreProviders {
		assert.Contains(t, all, core, "should include core provider %s", core)
	}

	// Should include all aliases
	for name := range EachAlias() {
		assert.Contains(t, all, name, "should include alias %s", name)
	}

	// Total count should be core + aliases
	assert.Len(t, all, len(CoreProviders)+len(Aliases))
}

func TestIsKnownProvider(t *testing.T) {
	t.Parallel()

	// All core providers should be known
	for _, core := range CoreProviders {
		assert.True(t, IsKnownProvider(core), "core provider %s should be known", core)
	}

	// All aliases should be known
	for name := range EachAlias() {
		assert.True(t, IsKnownProvider(name), "alias %s should be known", name)
	}

	// Case-insensitive
	assert.True(t, IsKnownProvider("OpenAI"))
	assert.True(t, IsKnownProvider("ANTHROPIC"))

	// Unknown providers
	assert.False(t, IsKnownProvider("unknown"))
	assert.False(t, IsKnownProvider(""))
}
