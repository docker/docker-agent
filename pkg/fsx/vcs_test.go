package fsx

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeGitDir creates a minimal valid .git directory under dir.
func writeGitDir(t *testing.T, dir string) {
	t.Helper()
	gitDir := filepath.Join(dir, ".git")
	require.NoError(t, os.Mkdir(gitDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644))
}

func TestFindRepoRoot(t *testing.T) {
	t.Parallel()

	t.Run("regular repository", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeGitDir(t, dir)

		root, found := findRepoRoot(dir)
		require.True(t, found)

		resolved, err := filepath.EvalSymlinks(dir)
		require.NoError(t, err)
		assert.Equal(t, resolved, root, "root must be absolute with symlinks resolved")
	})

	t.Run("no repository", func(t *testing.T) {
		t.Parallel()
		_, found := findRepoRoot(t.TempDir())
		assert.False(t, found)
	})

	t.Run("subdirectory of a repository is not detected", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeGitDir(t, dir)
		sub := filepath.Join(dir, "sub")
		require.NoError(t, os.Mkdir(sub, 0o755))

		// Mirrors the previous git.PlainOpen behavior: no upward search.
		_, found := findRepoRoot(sub)
		assert.False(t, found)
	})

	t.Run("empty .git directory is not a repository", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o755))

		_, found := findRepoRoot(dir)
		assert.False(t, found)
	})

	t.Run("linked worktree gitfile", func(t *testing.T) {
		t.Parallel()
		main := t.TempDir()
		writeGitDir(t, main)
		worktreeGitDir := filepath.Join(main, ".git", "worktrees", "wt")
		require.NoError(t, os.MkdirAll(worktreeGitDir, 0o755))

		wt := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+worktreeGitDir+"\n"), 0o644))

		root, found := findRepoRoot(wt)
		require.True(t, found)
		resolved, err := filepath.EvalSymlinks(wt)
		require.NoError(t, err)
		assert.Equal(t, resolved, root)
	})

	t.Run("gitfile with relative gitdir", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "actual-git"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: actual-git\n"), 0o644))

		_, found := findRepoRoot(dir)
		assert.True(t, found)
	})

	t.Run("dangling gitfile is not a repository", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: /does/not/exist\n"), 0o644))

		_, found := findRepoRoot(dir)
		assert.False(t, found)
	})

	t.Run("malformed gitfile is not a repository", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".git"), []byte("not a gitfile"), 0o644))

		_, found := findRepoRoot(dir)
		assert.False(t, found)
	})

	t.Run("bare repository has no matcher", func(t *testing.T) {
		t.Parallel()
		bare := t.TempDir()
		// A bare repo is the git dir itself: no .git entry inside.
		require.NoError(t, os.WriteFile(filepath.Join(bare, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644))

		_, found := findRepoRoot(bare)
		assert.False(t, found)
	})
}

func TestNewVCSMatcher(t *testing.T) {
	t.Parallel()

	t.Run("no repository returns nil matcher and nil error", func(t *testing.T) {
		t.Parallel()
		m, err := NewVCSMatcher(t.TempDir())
		require.NoError(t, err)
		assert.Nil(t, m)
	})

	t.Run("loads gitignore patterns", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeGitDir(t, dir)
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored.txt\n"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "ignored.txt"), nil, 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "kept.txt"), nil, 0o644))

		m, err := NewVCSMatcher(dir)
		require.NoError(t, err)
		require.NotNil(t, m)

		assert.True(t, m.ShouldIgnore(filepath.Join(dir, "ignored.txt")))
		assert.False(t, m.ShouldIgnore(filepath.Join(dir, "kept.txt")))
		assert.True(t, m.ShouldIgnore(filepath.Join(dir, ".git")), ".git itself is always ignored")
	})

	t.Run("nested gitignore patterns", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeGitDir(t, dir)
		sub := filepath.Join(dir, "sub")
		require.NoError(t, os.Mkdir(sub, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(sub, ".gitignore"), []byte("*.log\n"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(sub, "debug.log"), nil, 0o644))

		m, err := NewVCSMatcher(dir)
		require.NoError(t, err)
		require.NotNil(t, m)

		assert.True(t, m.ShouldIgnore(filepath.Join(sub, "debug.log")))
	})

	// A path outside the repository must never be matched against the
	// repository's patterns. A plain string-prefix containment test has no
	// path-component boundary, so "<root>-sibling" looks like it lives under
	// "<root>" and filepath.Rel yields "../<root>-sibling/...", whose trailing
	// component then matches a basename pattern such as "*.log".
	t.Run("sibling directory sharing the root name prefix is outside the repository", func(t *testing.T) {
		t.Parallel()
		parent := t.TempDir()

		repo := filepath.Join(parent, "repo")
		require.NoError(t, os.Mkdir(repo, 0o755))
		writeGitDir(t, repo)
		require.NoError(t, os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("*.log\n"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(repo, "app.log"), nil, 0o644))

		sibling := filepath.Join(parent, "repo-sibling")
		require.NoError(t, os.Mkdir(sibling, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(sibling, "app.log"), nil, 0o644))

		unrelated := filepath.Join(parent, "other")
		require.NoError(t, os.Mkdir(unrelated, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(unrelated, "app.log"), nil, 0o644))

		m, err := NewVCSMatcher(repo)
		require.NoError(t, err)
		require.NotNil(t, m)

		assert.True(t, m.ShouldIgnore(filepath.Join(repo, "app.log")),
			"a matching file inside the repository is still ignored")
		assert.False(t, m.ShouldIgnore(filepath.Join(sibling, "app.log")),
			"a file in a sibling directory is outside the repository")
		assert.False(t, m.ShouldIgnore(filepath.Join(unrelated, "app.log")),
			"a file in an unrelated directory is outside the repository")
	})

	t.Run("repository root itself is matched against its own patterns", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeGitDir(t, dir)
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*.log\n"), 0o644))

		m, err := NewVCSMatcher(dir)
		require.NoError(t, err)
		require.NotNil(t, m)

		assert.False(t, m.ShouldIgnore(dir), "the root is not itself ignored")
	})
}
