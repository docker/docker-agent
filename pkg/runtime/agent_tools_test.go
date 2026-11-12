package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/runtime/inspection"
)

// TestUniqueNames verifies the helper backing AgentConfigInfo: it drops empty
// names and de-duplicates, preserving first-seen order by default (so
// declaration/priority order survives) and sorting case-insensitively when
// requested, keeping the agent-details config lines stable and readable.
func TestUniqueNames(t *testing.T) {
	t.Parallel()

	// Default: first-seen order preserved, empties dropped, duplicates removed.
	got := inspection.UniqueNames([]string{"shell", "", "filesystem", "shell", "git"}, false)
	assert.Equal(t, []string{"shell", "filesystem", "git"}, got)

	// Sorted: case-insensitive ordering after de-duping. A lowercase name sorts
	// before an uppercase one alphabetically, unlike a raw byte comparison.
	sorted := inspection.UniqueNames([]string{"Zebra", "apple", "apple"}, true)
	assert.Equal(t, []string{"apple", "Zebra"}, sorted)

	assert.Empty(t, inspection.UniqueNames(nil, false))
	assert.Empty(t, inspection.UniqueNames([]string{"", ""}, true))
}
