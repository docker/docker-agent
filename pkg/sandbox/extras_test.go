package sandbox

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCleanExtras_DedupesEquivalentPaths(t *testing.T) {
	t.Parallel()

	// Two textually different paths that resolve to the same
	// directory must collapse to a single mount entry — otherwise
	// the duplicate triggers an unnecessary sandbox tear-down /
	// recreate cycle every run, since the on-disk sandbox shows the
	// path only once but the in-memory request would expect both.
	wd := mustAbs(t, t.TempDir())
	extra := mustAbs(t, t.TempDir())

	got, err := cleanExtras([]string{
		extra,
		extra + "/.",
		extra + "/./",
		filepath.Join(extra, "sub", ".."),
	}, wd)
	require.NoError(t, err)
	assert.Equal(t, []string{extra}, got)
}

func TestCleanExtras_FiltersEmptyAndWorkspaceMatches(t *testing.T) {
	t.Parallel()

	wd := mustAbs(t, t.TempDir())
	extra := mustAbs(t, t.TempDir())

	got, err := cleanExtras([]string{"", wd, wd + "/.", extra}, wd)
	require.NoError(t, err)
	assert.Equal(t, []string{extra}, got,
		"the workspace itself and empty entries must be dropped: only %q should remain", extra)
}

func TestCleanExtras_PreservesOrder(t *testing.T) {
	t.Parallel()

	wd := mustAbs(t, t.TempDir())
	a := mustAbs(t, t.TempDir())
	b := mustAbs(t, t.TempDir())
	c := mustAbs(t, t.TempDir())

	got, err := cleanExtras([]string{a, b, b, c, a}, wd)
	require.NoError(t, err)
	assert.Equal(t, []string{a, b, c}, got, "first occurrence wins, later duplicates drop")
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	abs, err := CanonicalPath(p)
	require.NoError(t, err)
	return filepath.Clean(abs)
}
