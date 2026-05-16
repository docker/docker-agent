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
