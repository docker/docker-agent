package board

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newLocalRepo creates a git repository with no remotes.
func newLocalRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	return dir
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	args = append([]string{"-c", "user.email=test@test", "-c", "user.name=test", "-c", "commit.gpgsign=false"}, args...)
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}

func TestUpstreamBaseFallsBackToLocalBranch(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	dir := newLocalRepo(t)
	// Before the first commit no branch resolves: fall back to HEAD.
	assert.Equal(t, "HEAD", upstreamBase(ctx, dir))

	require.NoError(t, os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hi\n"), 0o644))
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "initial")

	// No remotes: fall back to the local default branch.
	assert.Equal(t, "main", upstreamBase(ctx, dir))
}

func TestWorktreeDiffInLocalOnlyRepo(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	dir := newLocalRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hi\n"), 0o644))
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "initial")

	// Untracked and modified files show up even without any remote.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "new.txt"), []byte("new\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f.txt"), []byte("changed\n"), 0o644))

	diff, err := worktreeDiff(ctx, dir)
	require.NoError(t, err)
	assert.Contains(t, diff, "new.txt")
	assert.Contains(t, diff, "changed")

	// A missing worktree is still reported as an empty diff.
	diff, err = worktreeDiff(ctx, filepath.Join(dir, "missing"))
	require.NoError(t, err)
	assert.Empty(t, diff)
}

func TestCopyIndex(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	dir := newLocalRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hi\n"), 0o644))
	git(t, dir, "add", ".")

	indexCopy, cleanup, err := copyIndex(ctx, dir)
	require.NoError(t, err)

	want, err := os.ReadFile(filepath.Join(dir, ".git", "index"))
	require.NoError(t, err)
	got, err := os.ReadFile(indexCopy)
	require.NoError(t, err)
	assert.Equal(t, want, got)

	// Unix mode bits are not enforced on Windows.
	if runtime.GOOS != "windows" {
		info, err := os.Stat(indexCopy)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "index copy must not be group- or world-readable")
	}

	cleanup()
	_, err = os.Stat(indexCopy)
	assert.True(t, os.IsNotExist(err))
}

func TestCopyIndexMissingSource(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// A fresh repository has no index file until the first git add.
	dir := newLocalRepo(t)

	indexCopy, cleanup, err := copyIndex(ctx, dir)
	require.NoError(t, err)
	defer cleanup()

	got, err := os.ReadFile(indexCopy)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestUpstreamRemote(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		remotes []string
		want    string
	}{
		{"no remotes", nil, "origin"},
		{"origin only", []string{"origin"}, "origin"},
		{"upstream only", []string{"upstream"}, "upstream"},
		{"upstream after origin", []string{"origin", "upstream"}, "upstream"},
		{"upstream before other remote", []string{"upstream", "zebra"}, "upstream"},
		{"exact name required", []string{"not-upstream", "upstream-other"}, "origin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := newLocalRepo(t)
			for _, remote := range tc.remotes {
				git(t, dir, "remote", "add", remote, "https://example.com/repo.git")
			}
			assert.Equal(t, tc.want, upstreamRemote(t.Context(), dir))
		})
	}
	t.Run("not a repository", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, "origin", upstreamRemote(t.Context(), t.TempDir()))
	})
}
