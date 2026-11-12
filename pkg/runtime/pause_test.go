package runtime

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTogglePause_StateCycles verifies a /pause /pause /pause sequence
// alternates between paused and resumed.
func TestTogglePause_StateCycles(t *testing.T) {
	t.Parallel()

	r := &LocalRuntime{}

	assertToggle := func(want bool, msg string) {
		t.Helper()
		got, err := r.TogglePause(t.Context())
		require.NoError(t, err)
		assert.Equal(t, want, got, msg)
	}

	assertToggle(true, "first toggle should pause")
	assertToggle(false, "second toggle should resume")
	assertToggle(true, "third toggle should pause again")
	assertToggle(false, "fourth toggle should resume again")
}

// TestIsPaused_TracksToggle verifies isPaused mirrors the armed state set by
// TogglePause.
func TestIsPaused_TracksToggle(t *testing.T) {
	t.Parallel()

	r := &LocalRuntime{}
	assert.False(t, r.isPaused(), "should not be paused initially")

	_, _ = r.TogglePause(t.Context())
	assert.True(t, r.isPaused(), "should be paused after first toggle")

	_, _ = r.TogglePause(t.Context())
	assert.False(t, r.isPaused(), "should not be paused after second toggle")
}

// TestWaitIfPaused_NotPaused returns immediately when the runtime isn't paused.
func TestWaitIfPaused_NotPaused(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		r := &LocalRuntime{}

		done := make(chan error, 1)
		go func() { done <- r.waitIfPaused(t.Context()) }()

		synctest.Wait()

		select {
		case err := <-done:
			require.NoError(t, err)
		default:
			t.Fatal("waitIfPaused should return immediately when not paused")
		}
	})
}

// TestWaitIfPaused_BlocksUntilResumed verifies that a goroutine in
// waitIfPaused stays blocked while paused and wakes up on resume.
func TestWaitIfPaused_BlocksUntilResumed(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		r := &LocalRuntime{}
		_, _ = r.TogglePause(t.Context()) // pause

		done := make(chan error, 1)
		go func() { done <- r.waitIfPaused(t.Context()) }()

		synctest.Wait()
		synctest.Sleep(50 * time.Millisecond)

		select {
		case <-done:
			t.Fatal("waitIfPaused returned before resume")
		default:
		}

		_, _ = r.TogglePause(t.Context()) // resume
		synctest.Wait()

		select {
		case err := <-done:
			require.NoError(t, err)
		default:
			t.Fatal("waitIfPaused did not unblock after resume")
		}
	})
}

// TestWaitIfPaused_ContextCancellation verifies cancelling the context wakes
// up a goroutine waiting in waitIfPaused, returning the context error.
func TestWaitIfPaused_ContextCancellation(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		r := &LocalRuntime{}
		_, _ = r.TogglePause(t.Context()) // pause

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- r.waitIfPaused(ctx) }()

		synctest.Wait()
		synctest.Sleep(50 * time.Millisecond)

		select {
		case <-done:
			t.Fatal("waitIfPaused returned before cancellation")
		default:
		}

		cancel()
		synctest.Wait()

		select {
		case err := <-done:
			require.ErrorIs(t, err, context.Canceled)
		default:
			t.Fatal("waitIfPaused did not unblock after ctx cancellation")
		}
	})
}

// TestWaitIfPaused_BroadcastsToAllWaiters verifies a single resume wakes up
// every goroutine that was waiting on the same pause.
func TestWaitIfPaused_BroadcastsToAllWaiters(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		r := &LocalRuntime{}
		_, _ = r.TogglePause(t.Context())

		const n = 8
		var wg sync.WaitGroup
		for range n {
			wg.Go(func() {
				_ = r.waitIfPaused(t.Context())
			})
		}

		// All n waiters are durably parked on the pause channel now.
		synctest.Wait()

		_, _ = r.TogglePause(t.Context()) // single resume must wake all waiters

		// A partial wake-up leaves waiters parked forever, which synctest
		// reports as a deadlock instead of hanging the suite.
		wg.Wait()
	})
}

// TestTogglePause_RaceFreeUnderConcurrentCallers exercises concurrent
// TogglePause and waitIfPaused calls. Run with -race to detect data races.
func TestTogglePause_RaceFreeUnderConcurrentCallers(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		r := &LocalRuntime{}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		var wg sync.WaitGroup
		const togglers = 4
		const waiters = 4

		for range togglers {
			wg.Go(func() {
				for range 200 {
					_, _ = r.TogglePause(ctx)
				}
			})
		}
		for range waiters {
			wg.Go(func() {
				for range 200 {
					_ = r.waitIfPaused(ctx)
				}
			})
		}

		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()

		// If a waiter is left blocked on a pause that no toggler will flip,
		// cancelling the context unblocks it so wg.Wait() can return.
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			cancel()
			<-done
		}
	})
}
