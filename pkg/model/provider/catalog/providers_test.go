package catalog

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsCatalogProvider(t *testing.T) {
	t.Parallel()

	for name, p := range Providers {
		assert.Equal(t, !p.RequiresConfiguration, IsCatalogProvider(name), name)
	}
	assert.False(t, IsCatalogProvider("unknown"))
}

func TestAllProviders(t *testing.T) {
	t.Parallel()

	assert.Equal(t, slices.Sorted(maps.Keys(Providers)), AllProviders())
}

func TestIsKnownProvider(t *testing.T) {
	t.Parallel()

	for name := range Providers {
		assert.True(t, IsKnownProvider(name), name)
		assert.True(t, IsKnownProvider(strings.ToUpper(name)), name)
	}
	assert.False(t, IsKnownProvider("unknown"))
	assert.False(t, IsKnownProvider(""))
}
