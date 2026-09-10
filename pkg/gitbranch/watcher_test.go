package gitbranch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWatcherReportsBranchChanges(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git")
	require.NoError(t, os.Mkdir(gitDir, 0o755))
	head := filepath.Join(gitDir, "HEAD")
	require.NoError(t, os.WriteFile(head, []byte("ref: refs/heads/main\n"), 0o644))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	watcher, err := Watch(ctx, dir)
	require.NoError(t, err)
	assert.Equal(t, "main", watcher.Current())

	require.NoError(t, os.WriteFile(head, []byte("ref: refs/heads/feature/live-footer\n"), 0o644))
	select {
	case branch := <-watcher.Changes():
		assert.Equal(t, "feature/live-footer", branch)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for branch change")
	}
}

func TestWatcherSetDir(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	watcher, err := Watch(ctx, t.TempDir())
	require.NoError(t, err)

	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/other\n"), 0o644))

	assert.Equal(t, "other", watcher.SetDir(dir))
	assert.Equal(t, "other", watcher.Current())
}
