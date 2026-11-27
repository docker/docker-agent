package worktree

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestAbsoluteCommonDirRejectsOlderGitEcho pins that the --path-format=absolute
// probe's output is only used when it is a single absolute path: git 2.30
// echoes the unknown flag and exits 0, leaving a flag line followed by the
// relative directory. Using that as a lock key would give a main checkout and
// a linked checkout different identities, so the probe must fall back to the
// relative form instead.
func TestAbsoluteCommonDirRejectsOlderGitEcho(t *testing.T) {
	t.Parallel()

	// Modern git: one absolute path.
	abs := t.TempDir()
	got, ok := absoluteCommonDir(abs)
	assert.True(t, ok)
	assert.Equal(t, abs, got)

	// Older git echoes the flag and prints the relative directory.
	_, ok = absoluteCommonDir("--path-format=absolute\n.git")
	assert.False(t, ok, "an echoed-flag multi-line response must not be used as a lock key")

	// Relative or empty outputs are not usable either.
	_, ok = absoluteCommonDir(".git")
	assert.False(t, ok)
	_, ok = absoluteCommonDir("")
	assert.False(t, ok)
}
