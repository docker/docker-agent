package tools

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestWithoutSearchOnly(t *testing.T) {
	t.Parallel()

	regular := []Tool{{Name: "read"}, {Name: "write"}}
	assert.Equal(t, regular, WithoutSearchOnly(regular))
	assert.Nil(t, WithoutSearchOnly(nil))

	mixed := []Tool{{Name: "read"}, {Name: "search", InCatalog: true, SearchOnly: true}, {Name: "write"}}
	filtered := WithoutSearchOnly(mixed)
	assert.Equal(t, regular, filtered)
	assert.Len(t, mixed, 3, "input must not be mutated")

	// A listed catalog tool (e.g. activated through add_tool) is a regular
	// tool for providers without hosted tool search.
	listed := []Tool{{Name: "read"}, {Name: "activated", InCatalog: true}}
	assert.Equal(t, listed, WithoutSearchOnly(listed))
}
