package snapshot

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTrackPatchAndRevert(t *testing.T) {
	dir := bootstrapRepo(t)
	repo := openRepo(t, dir)

	before, err := repo.Track(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, before)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("modified"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "new.txt"), []byte("new"), 0o644))
	require.NoError(t, os.Remove(filepath.Join(dir, "b.txt")))

	patch, err := repo.Patch(t.Context(), before)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{
		filepath.ToSlash(filepath.Join(repo.Worktree(), "a.txt")),
		filepath.ToSlash(filepath.Join(repo.Worktree(), "b.txt")),
		filepath.ToSlash(filepath.Join(repo.Worktree(), "new.txt")),
	}, patch.Files)

	require.NoError(t, repo.Revert(t.Context(), []Patch{patch}))
	assertFile(t, filepath.Join(dir, "a.txt"), "A")
	assertFile(t, filepath.Join(dir, "b.txt"), "B")
	assert.NoFileExists(t, filepath.Join(dir, "new.txt"))
}

func TestGitignoreAndLargeFilesAreNotSnapshotted(t *testing.T) {
	dir := bootstrapRepo(t)
	repo := openRepo(t, dir)

	before, err := repo.Track(t.Context())
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*.ignored\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "skip.ignored"), []byte("ignored"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "huge.bin"), make([]byte, largeFileLimit+1), 0o644))

	patch, err := repo.Patch(t.Context(), before)
	require.NoError(t, err)
	assert.Contains(t, patch.Files, filepath.ToSlash(filepath.Join(repo.Worktree(), ".gitignore")))
	assert.NotContains(t, patch.Files, filepath.ToSlash(filepath.Join(repo.Worktree(), "skip.ignored")))
	assert.NotContains(t, patch.Files, filepath.ToSlash(filepath.Join(repo.Worktree(), "huge.bin")))

	after, err := repo.Track(t.Context())
	require.NoError(t, err)
	diffs, err := repo.DiffFull(t.Context(), before, after)
	require.NoError(t, err)
	for _, diff := range diffs {
		assert.NotEqual(t, "skip.ignored", diff.File)
		assert.NotEqual(t, "huge.bin", diff.File)
	}
}

func TestDiffFullReportsFileMetadata(t *testing.T) {
	dir := bootstrapRepo(t)
	repo := openRepo(t, dir)

	before, err := repo.Track(t.Context())
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("A\nchanged\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "new.txt"), []byte("new\n"), 0o644))
	require.NoError(t, os.Remove(filepath.Join(dir, "b.txt")))

	after, err := repo.Track(t.Context())
	require.NoError(t, err)

	diffs, err := repo.DiffFull(t.Context(), before, after)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"a.txt", "b.txt", "new.txt"}, diffFiles(diffs))
	assert.Equal(t, "modified", diffByFile(diffs, "a.txt").Status)
	assert.Equal(t, "deleted", diffByFile(diffs, "b.txt").Status)
	assert.Equal(t, "added", diffByFile(diffs, "new.txt").Status)
	assert.Contains(t, diffByFile(diffs, "a.txt").Patch, "changed")
}

func TestInvalidHashPatchIsEmpty(t *testing.T) {
	dir := bootstrapRepo(t)
	repo := openRepo(t, dir)

	_, err := repo.Track(t.Context())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "changed.txt"), []byte("changed"), 0o644))

	patch, err := repo.Patch(t.Context(), "not-a-real-hash")
	require.NoError(t, err)
	assert.Equal(t, "not-a-real-hash", patch.Hash)
	assert.Empty(t, patch.Files)
}

func TestOpenOutsideGitRepo(t *testing.T) {
	_, err := NewManager(t.TempDir()).Open(t.Context(), t.TempDir())
	assert.ErrorIs(t, err, ErrNotGitRepository)
}

func bootstrapRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test User")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("A"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.txt"), []byte("B"), 0o644))
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "init")
	return dir
}

func openRepo(t *testing.T, dir string) *Repo {
	t.Helper()
	repo, err := NewManager(t.TempDir()).Open(t.Context(), dir)
	require.NoError(t, err)
	return repo
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
}

func diffFiles(diffs []FileDiff) []string {
	files := make([]string, 0, len(diffs))
	for _, diff := range diffs {
		files = append(files, diff.File)
	}
	return files
}

func diffByFile(diffs []FileDiff, file string) FileDiff {
	for _, diff := range diffs {
		if diff.File == file {
			return diff
		}
	}
	return FileDiff{}
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, want, string(got))
}
