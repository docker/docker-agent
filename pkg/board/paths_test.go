package board

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/testutil"
)

func TestContractHome(t *testing.T) {
	t.Parallel()

	home := paths.GetHomeDir()

	assert.Equal(t, "~/src/repo", contractHome(filepath.Join(home, "src", "repo")))
	assert.Equal(t, "~", contractHome(home))
	assert.Empty(t, contractHome(""))
	// A path outside home is stored as-is.
	assert.Equal(t, filepath.FromSlash("/opt/repo"), contractHome(filepath.FromSlash("/opt/repo")))
	// A sibling of home must not lose its prefix.
	assert.Equal(t, home+"-other", contractHome(home+"-other"))
}

func TestExpandHome(t *testing.T) {
	t.Parallel()

	home := paths.GetHomeDir()

	assert.Equal(t, filepath.Join(home, "src", "repo"), expandHome("~/src/repo"))
	assert.Equal(t, home, expandHome("~"))
	assert.Empty(t, expandHome(""))
	assert.Equal(t, filepath.FromSlash("/opt/repo"), expandHome(filepath.FromSlash("/opt/repo")))
	// Only a leading ~/ is expanded, not ~ elsewhere or ~user.
	assert.Equal(t, "~user/src", expandHome("~user/src"))
}

// TestExpandHomeNoHome proves a failed home lookup leaves "~" paths
// untouched instead of turning them into relative paths that a later save
// would persist.
func TestExpandHomeNoHome(t *testing.T) {
	testutil.SetHome(t, "")

	assert.Equal(t, "~/src/repo", expandHome("~/src/repo"))
	assert.Equal(t, "~", expandHome("~"))
}

func TestContractExpandRoundTrip(t *testing.T) {
	t.Parallel()

	p := filepath.Join(paths.GetHomeDir(), ".cagent", "worktrees", "board-abc")
	assert.Equal(t, p, expandHome(contractHome(p)))
}
