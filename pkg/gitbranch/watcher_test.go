package gitbranch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
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

func TestWatcherRefreshReplacesPendingChange(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/latest\n"), 0o644))
	watcher := &Watcher{dir: dir, current: "old", changes: make(chan string, 1)}
	watcher.changes <- "stale"

	watcher.refresh()

	assert.Equal(t, "latest", watcher.Current())
	assert.Equal(t, "latest", <-watcher.Changes())
	watcher.refresh()
	assert.Empty(t, watcher.changes, "unchanged branches must not be published again")
}

func TestWatcherRefreshConcurrentConsumer(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		// An empty dir avoids filesystem I/O while stressing the notification channel.
		watcher := &Watcher{changes: make(chan string, 1)}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-watcher.Changes():
				}
			}
		}()
		done := make(chan struct{})
		go func() {
			defer close(done)
			for range 10000 {
				watcher.mu.Lock()
				watcher.current = "old"
				watcher.mu.Unlock()
				watcher.refresh()
			}
		}()

		<-done
	})
}
