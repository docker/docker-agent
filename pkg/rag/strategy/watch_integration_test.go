//go:build !js && !wasip1

package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/fsx"
	"github.com/docker/docker-agent/pkg/rag/database"
)

func TestStrategiesWatchFiles(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"bm25", "vector"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			dir, err := filepath.EvalSymlinks(t.TempDir())
			require.NoError(t, err)
			path := filepath.Join(dir, "document.txt")
			require.NoError(t, os.WriteFile(path, []byte("original document content"), 0o600))
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			var loop func() error
			var metadata func() (*database.FileMetadata, error)
			if kind == "bm25" {
				s := newBM25Strategy("test", newMemoryBM25DB(), nil, 1.5, 0.75, ChunkingConfig{Size: 1024}, nil)
				t.Cleanup(func() { require.NoError(t, s.Close()) })
				require.NoError(t, s.Initialize(ctx, []string{dir}, ChunkingConfig{}))
				loop = func() error { return s.WatchFiles(ctx, []string{dir}, ChunkingConfig{}) }
				metadata = func() (*database.FileMetadata, error) { return s.db.GetFileMetadata(t.Context(), path) }
			} else {
				s := newVectorStoreWithDB(&fakeEmbeddingProvider{}, newFakeVectorDB())
				t.Cleanup(func() { require.NoError(t, s.Close()) })
				require.NoError(t, s.Initialize(ctx, []string{dir}, ChunkingConfig{}))
				loop = func() error { return s.WatchFiles(ctx, []string{dir}, ChunkingConfig{}) }
				metadata = func() (*database.FileMetadata, error) { return s.db.GetFileMetadata(t.Context(), path) }
			}
			before, err := metadata()
			require.NoError(t, err)
			require.NotNil(t, before)
			go func() { done <- loop() }()
			t.Cleanup(func() {
				cancel()
				select {
				case err := <-done:
					require.ErrorIs(t, err, context.Canceled)
				case <-time.After(5 * time.Second):
					t.Error("watcher failed to stop")
				}
			})
			lastWrite := time.Time{}

			require.Eventually(t, func() bool {
				// WatchFiles intentionally has no readiness contract. Retry after
				// a full debounce window if the first write preceded attachment.
				if time.Since(lastWrite) > 3*time.Second {
					require.NoError(t, os.WriteFile(path, []byte("updated document content"), 0o600))
					lastWrite = time.Now()
				}
				after, err := metadata()
				return err == nil && after != nil && after.FileHash != before.FileHash
			}, 10*time.Second, 10*time.Millisecond)
		})
	}
}

func TestWatchFileChangesReconcilesCreatedDirectory(t *testing.T) {
	t.Parallel()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	watcher := fakeFileWatcher()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	found := make(chan []string, 1)
	done := make(chan error, 1)
	go func() {
		done <- watchFileChanges(ctx, watcher, "test", []string{dir}, nil,
			func(context.Context, string) error { return nil },
			func(context.Context, []string) { close(entered); <-release },
			func(ctx context.Context) error {
				files, err := fsx.CollectFiles(ctx, []string{dir}, nil)
				found <- files
				return err
			}, func() error { return nil })
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("watcher failed to stop")
		}
	})
	initial := filepath.Join(dir, "a.txt")
	require.NoError(t, os.WriteFile(initial, []byte("before"), 0o600))
	watcher.Events <- fsnotify.Event{Name: initial, Op: fsnotify.Write}
	<-entered
	childDir := filepath.Join(dir, "new")
	require.NoError(t, os.Mkdir(childDir, 0o700))
	watcher.Events <- fsnotify.Event{Name: childDir, Op: fsnotify.Create}
	target := filepath.Join(childDir, "b.txt")
	require.NoError(t, os.WriteFile(target, []byte("new content"), 0o600))
	// No child event: its parent was not watched while the first batch was busy.
	close(release)
	select {
	case files := <-found:
		require.Contains(t, files, target)
	case <-time.After(10 * time.Second):
		t.Fatal("new directory contents were not reconciled")
	}
}
