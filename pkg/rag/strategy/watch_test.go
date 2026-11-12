package strategy

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fakeFileWatcher() *fsnotify.Watcher {
	return &fsnotify.Watcher{Events: make(chan fsnotify.Event, 16), Errors: make(chan error, 1)}
}

func TestWatchFileChangesDebouncesAndFilters(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		watcher := fakeFileWatcher()
		batches := make(chan []string, 4)
		added := make(chan string, 4)
		done := make(chan error, 1)
		go func() {
			done <- watchFileChanges(ctx, watcher, "test", []string{"/docs/*.txt"},
				func(path string) bool { return path == "/docs/ignored.txt" },
				func(_ context.Context, path string) error { added <- path; return nil },
				func(_ context.Context, files []string) { batches <- files }, func(context.Context) error { return nil }, func() error { return nil })
		}()

		watcher.Events <- fsnotify.Event{Name: "/docs/b.txt", Op: fsnotify.Write}
		watcher.Events <- fsnotify.Event{Name: "/docs/b.txt", Op: fsnotify.Write}
		watcher.Events <- fsnotify.Event{Name: "/docs/a.txt", Op: fsnotify.Create}
		watcher.Events <- fsnotify.Event{Name: "/docs/ignored.txt", Op: fsnotify.Write}
		watcher.Events <- fsnotify.Event{Name: "/docs/no.md", Op: fsnotify.Write}
		watcher.Events <- fsnotify.Event{Name: "/docs/chmod.txt", Op: fsnotify.Chmod}
		synctest.Wait()
		assert.Empty(t, batches)
		assert.Equal(t, "/docs/*.txt", <-added)
		assert.Equal(t, "/docs/a.txt", <-added)
		time.Sleep(fileWatchDebounce) //nolint:forbidigo // Advance the synctest clock.
		synctest.Wait()
		assert.Equal(t, []string{"/docs/a.txt", "/docs/b.txt"}, <-batches)
		assert.Empty(t, batches)
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
	})
}

func TestWatchFileChangesCancellationDiscardsPending(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		watcher := fakeFileWatcher()
		var processed atomic.Int32
		done := make(chan error, 1)
		go func() {
			done <- watchFileChanges(ctx, watcher, "test", []string{"/docs/*.txt"}, nil,
				func(context.Context, string) error { return nil }, func(context.Context, []string) { processed.Add(1) }, func(context.Context) error { return nil }, func() error { return nil })
		}()
		watcher.Events <- fsnotify.Event{Name: "/docs/a.txt", Op: fsnotify.Write}
		synctest.Wait()
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
		time.Sleep(2 * fileWatchDebounce) //nolint:forbidigo // Verify stopped timers cannot fire later.
		assert.Zero(t, processed.Load())
	})
}

func TestWatchFileChangesJoinsProcessing(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		watcher := fakeFileWatcher()
		started, release := make(chan struct{}), make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- watchFileChanges(ctx, watcher, "test", []string{"/docs/*.txt"}, nil,
				func(context.Context, string) error { return nil }, func(context.Context, []string) {
					close(started)
					<-release
				}, func(context.Context) error { return nil }, func() error { return nil })
		}()
		watcher.Events <- fsnotify.Event{Name: "/docs/a.txt", Op: fsnotify.Write}
		<-started
		cancel()
		synctest.Wait()
		assert.Empty(t, done, "return must wait for in-flight indexing")
		close(release)
		require.ErrorIs(t, <-done, context.Canceled)
	})
}

func TestWatchFileChangesOverflow(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		watcher := fakeFileWatcher()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		failure := errors.New("rescan failed")
		rescanned := make(chan struct{})
		var added []string
		done := make(chan error, 1)
		go func() {
			done <- watchFileChanges(ctx, watcher, "test", []string{"/docs/*.txt"}, nil,
				func(_ context.Context, path string) error { added = append(added, path); return nil },
				func(context.Context, []string) { t.Error("unexpected batch") }, func(context.Context) error { close(rescanned); return failure }, func() error { return nil })
		}()
		watcher.Errors <- fsnotify.ErrEventOverflow
		<-rescanned
		synctest.Wait()
		assert.Empty(t, done, "indexing failures must not stop watching")
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
		assert.Equal(t, []string{"/docs/*.txt", "/docs/*.txt"}, added)
	})
}

func TestWatchFileChangesClosedChannels(t *testing.T) {
	t.Parallel()
	for _, events := range []bool{true, false} {
		t.Run(map[bool]string{true: "events", false: "errors"}[events], func(t *testing.T) {
			t.Parallel()
			watcher := fakeFileWatcher()
			if events {
				close(watcher.Events)
			} else {
				close(watcher.Errors)
			}
			err := watchFileChanges(t.Context(), watcher, "test", nil, nil,
				func(context.Context, string) error { return nil }, func(context.Context, []string) {}, func(context.Context) error { return nil }, func() error { return nil })
			require.ErrorContains(t, err, "closed unexpectedly")
		})
	}
}

func TestWatchFileChangesQueuesEditsDuringIndexing(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		watcher := fakeFileWatcher()
		batches := make(chan []string, 2)
		release := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- watchFileChanges(ctx, watcher, "test", []string{"/docs/*.txt"}, nil,
				func(context.Context, string) error { return nil }, func(_ context.Context, files []string) {
					batches <- files
					<-release
				}, func(context.Context) error { return nil }, func() error { return nil })
		}()
		watcher.Events <- fsnotify.Event{Name: "/docs/a.txt", Op: fsnotify.Write}
		assert.Equal(t, []string{"/docs/a.txt"}, <-batches)
		watcher.Events <- fsnotify.Event{Name: "/docs/b.txt", Op: fsnotify.Write}
		watcher.Events <- fsnotify.Event{Name: "/docs/b.txt", Op: fsnotify.Write}
		synctest.Wait()
		assert.Empty(t, batches, "batches must never overlap")
		close(release)
		assert.Equal(t, []string{"/docs/b.txt"}, <-batches)
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
	})
}

func TestWatchFileChangesExtendsDebounce(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		watcher := fakeFileWatcher()
		batches := make(chan []string, 1)
		done := make(chan error, 1)
		go func() {
			done <- watchFileChanges(ctx, watcher, "test", []string{"/docs/*.txt"}, nil,
				func(context.Context, string) error { return nil }, func(_ context.Context, files []string) { batches <- files }, func(context.Context) error { return nil }, func() error { return nil })
		}()
		watcher.Events <- fsnotify.Event{Name: "/docs/a.txt", Op: fsnotify.Write}
		synctest.Wait()
		time.Sleep(time.Second) //nolint:forbidigo // Advance fake time midway through the debounce.
		watcher.Events <- fsnotify.Event{Name: "/docs/b.txt", Op: fsnotify.Write}
		synctest.Wait()
		time.Sleep(time.Second) //nolint:forbidigo // The original deadline must no longer fire.
		synctest.Wait()
		assert.Empty(t, batches)
		time.Sleep(time.Second) //nolint:forbidigo // Reach the reset deadline.
		assert.Equal(t, []string{"/docs/a.txt", "/docs/b.txt"}, <-batches)
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
	})
}

func TestWatchFilesAlreadyCanceled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, (&BM25Strategy{}).WatchFiles(ctx, nil, ChunkingConfig{}), context.Canceled)
	require.ErrorIs(t, (&VectorStore{}).WatchFiles(ctx, nil, ChunkingConfig{}), context.Canceled)
}

func TestWatchFileChangesDrainsDuringRegistration(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		watcher := fakeFileWatcher()
		addStarted := make(chan struct{})
		backendReleased := make(chan struct{})
		closed := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- watchFileChanges(ctx, watcher, "test", []string{"/docs/*.txt"}, nil,
				func(ctx context.Context, _ string) error {
					close(addStarted)
					select {
					case <-backendReleased:
						return nil
					case <-closed:
						return ctx.Err()
					}
				}, func(context.Context, []string) {}, func(context.Context) error { return nil },
				func() error { close(closed); return nil })
		}()
		<-addStarted
		go func() {
			for range 100 {
				watcher.Events <- fsnotify.Event{Name: "/docs/a.txt", Op: fsnotify.Write}
			}
			close(backendReleased)
		}()
		<-backendReleased
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
	})
}

func TestWatchFileChangesDrainsBeforeClosingRegistration(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		watcher := fakeFileWatcher()
		started, backendRelease := make(chan struct{}), make(chan struct{})
		var registrationDone bool
		done := make(chan error, 1)
		go func() {
			done <- watchFileChanges(ctx, watcher, "test", []string{"/docs/*.txt"}, nil,
				func(context.Context, string) error {
					close(started)
					<-backendRelease
					registrationDone = true
					return nil
				},
				func(context.Context, []string) {}, func(context.Context) error { return nil },
				func() error { assert.True(t, registrationDone, "Close must not race backend Add reply"); return nil })
		}()
		<-started
		cancel()
		go func() {
			for range 100 {
				watcher.Events <- fsnotify.Event{Name: "/docs/a.txt", Op: fsnotify.Write}
			}
			close(backendRelease)
		}()
		require.ErrorIs(t, <-done, context.Canceled)
	})
}

func TestExcludedCreatesDoNotDelayPendingChanges(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		watcher := fakeFileWatcher()
		batches := make(chan []string, 4)
		done := make(chan error, 1)
		go func() {
			done <- watchFileChanges(ctx, watcher, "test", []string{"/docs/*.txt"},
				func(path string) bool { return path == "/docs/ignored.txt" },
				func(context.Context, string) error { return nil },
				func(_ context.Context, files []string) { batches <- files },
				func(context.Context) error { return nil }, func() error { return nil })
		}()
		watcher.Events <- fsnotify.Event{Name: "/docs/wanted.txt", Op: fsnotify.Write}
		synctest.Wait()
		time.Sleep(time.Second) //nolint:forbidigo // Advance fake time inside the debounce interval.
		watcher.Events <- fsnotify.Event{Name: "/docs/other.md", Op: fsnotify.Create}
		watcher.Events <- fsnotify.Event{Name: "/docs/ignored.txt", Op: fsnotify.Create}
		synctest.Wait()
		time.Sleep(time.Second) //nolint:forbidigo // The original matching-change deadline must still fire.
		synctest.Wait()
		assert.Equal(t, []string{"/docs/wanted.txt"}, <-batches)
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
	})
}

func TestWatchFileChangesOverflowRescansAndContinues(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		watcher := fakeFileWatcher()
		scanned := make(chan struct{}, 1)
		batches := make(chan []string, 1)
		done := make(chan error, 1)
		go func() {
			done <- watchFileChanges(ctx, watcher, "test", []string{"/docs/*.txt"}, nil,
				func(context.Context, string) error { return nil },
				func(_ context.Context, files []string) { batches <- files },
				func(context.Context) error { scanned <- struct{}{}; return nil }, func() error { return nil })
		}()
		watcher.Errors <- fsnotify.ErrEventOverflow
		<-scanned
		watcher.Events <- fsnotify.Event{Name: "/docs/next.txt", Op: fsnotify.Write}
		assert.Equal(t, []string{"/docs/next.txt"}, <-batches)
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
	})
}
