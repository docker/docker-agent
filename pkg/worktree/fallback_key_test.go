package worktree

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRepoCommonDirFallbackTiesAliases pins the compatibility path: when the
// absolute probe returns older git's echoed flag response, the fallback must
// give an aliased main checkout and a linked checkout the same lock key.
func TestRepoCommonDirFallbackTiesAliases(t *testing.T) {
	dir := bootstrapRepo(t)

	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	linked := filepath.Join(t.TempDir(), "linked")
	runGit(t, dir, "worktree", "add", "--detach", linked)

	prev := gitOutput
	t.Cleanup(func() { gitOutput = prev })
	gitOutput = func(ctx context.Context, dir string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "rev-parse" && args[1] == "--path-format=absolute" {
			// Older git echoes the unknown flag and exits 0.
			return "--path-format=absolute\n.git", nil
		}
		return prev(ctx, dir, args...)
	}

	mainKey, err := repoCommonDir(t.Context(), dir)
	require.NoError(t, err)
	aliasKey, err := repoCommonDir(t.Context(), alias)
	require.NoError(t, err)
	linkedKey, err := repoCommonDir(t.Context(), linked)
	require.NoError(t, err)

	require.Equal(t, mainKey, aliasKey, "an aliased main checkout must share the fallback's key")
	require.Equal(t, mainKey, linkedKey, "a linked checkout must share the fallback's key")
}
