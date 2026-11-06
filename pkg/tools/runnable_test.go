package tools_test

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

type runnableToolSet struct {
	run func(context.Context)
}

func (*runnableToolSet) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }
func (r *runnableToolSet) Run(ctx context.Context)                   { r.run(ctx) }

func TestRunnableDetachedStartup(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		type contextKey struct{}
		started := make(chan context.Context, 1)
		finished := make(chan struct{})
		s := tools.NewStartable(tools.WithName(&runnableToolSet{run: func(ctx context.Context) {
			defer close(finished)
			started <- ctx
			<-ctx.Done()
		}}, "worker"))
		t.Cleanup(func() { require.NoError(t, s.Stop(t.Context())) })
		ctx, cancel := context.WithTimeout(context.WithValue(t.Context(), contextKey{}, "value"), time.Second)
		defer cancel()
		ready, err := s.TryStartWithTimeout(ctx, time.Second)
		require.NoError(t, err)
		require.True(t, ready)
		runCtx := <-started
		cancel()
		synctest.Wait()
		require.NoError(t, runCtx.Err())
		assert.Equal(t, "value", runCtx.Value(contextKey{}))
		_, hasDeadline := runCtx.Deadline()
		assert.False(t, hasDeadline)
		require.NoError(t, s.Stop(ctx))
		<-finished
		require.ErrorIs(t, runCtx.Err(), context.Canceled)
		assert.False(t, s.IsStarted())
	})
}

func TestRunnableConcurrentLifecycle(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var runs, active atomic.Int32
		var overlap atomic.Bool
		inner := &runnableToolSet{run: func(ctx context.Context) {
			if active.Add(1) != 1 {
				overlap.Store(true)
			}
			defer active.Add(-1)
			runs.Add(1)
			<-ctx.Done()
		}}
		// Nested lifecycle wrappers must delegate instead of starting a second run.
		s := tools.NewStartable(tools.WithName(tools.NewStartable(inner), "worker"))
		t.Cleanup(func() { require.NoError(t, s.Stop(t.Context())) })
		for range 3 {
			var callers sync.WaitGroup
			for range 20 {
				callers.Go(func() { assert.NoError(t, s.Start(t.Context())) })
			}
			callers.Wait()
			synctest.Wait()
			assert.EqualValues(t, 1, active.Load())
			for range 20 {
				callers.Go(func() { assert.NoError(t, s.Stop(t.Context())) })
			}
			callers.Wait()
			assert.Zero(t, active.Load())
		}
		assert.EqualValues(t, 3, runs.Load())
		assert.False(t, overlap.Load())
	})
}

func TestRunnableStopJoinsBeforeRestart(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		started := make(chan struct{}, 2)
		cancelled, release := make(chan struct{}), make(chan struct{})
		var runs atomic.Int32
		s := tools.NewStartable(&runnableToolSet{run: func(ctx context.Context) {
			generation := runs.Add(1)
			started <- struct{}{}
			<-ctx.Done()
			if generation == 1 {
				close(cancelled)
				<-release
			}
		}})
		t.Cleanup(func() { require.NoError(t, s.Stop(t.Context())) })
		require.NoError(t, s.Start(t.Context()))
		<-started
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		stopped := make(chan error, 1)
		go func() { stopped <- s.Stop(ctx) }()
		<-cancelled
		restarted := make(chan error, 1)
		go func() { restarted <- s.Start(t.Context()) }()
		synctest.Wait()
		select {
		case err := <-stopped:
			t.Fatalf("Stop returned before the worker exited: %v", err)
		default:
		}
		assert.EqualValues(t, 1, runs.Load())
		close(release)
		require.NoError(t, <-stopped)
		require.NoError(t, <-restarted)
		<-started
		assert.EqualValues(t, 2, runs.Load())
	})
}

func TestRunnableCompletionUpdatesState(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var runs atomic.Int32
		release := make(chan struct{})
		s := tools.NewStartable(&runnableToolSet{run: func(context.Context) {
			runs.Add(1)
			<-release
		}})
		require.NoError(t, s.StopIfStarted(t.Context()))
		assert.Zero(t, runs.Load())
		require.NoError(t, s.Start(t.Context()))
		synctest.Wait()
		require.True(t, s.IsStarted())
		close(release)
		synctest.Wait()
		assert.False(t, s.IsStarted())
		assert.False(t, s.TryIsStarted())
		assert.False(t, s.TryIsHealthy())
		assert.False(t, s.TryIsAvailable())
		started, inFlight := s.TryState()
		assert.False(t, started)
		assert.False(t, inFlight)
		require.NoError(t, s.Start(t.Context()))
		synctest.Wait()
		assert.EqualValues(t, 2, runs.Load())
		require.NoError(t, s.StopIfStarted(t.Context()))
		require.NoError(t, s.StopIfStarted(t.Context()))
	})
}

type bothLifecycleToolSet struct {
	runnableToolSet

	starts, stops int
}

func (b *bothLifecycleToolSet) Start(context.Context) error { b.starts++; return nil }
func (b *bothLifecycleToolSet) Stop(context.Context) error  { b.stops++; return nil }

func TestRunnablePreservesStartableOwnership(t *testing.T) {
	t.Parallel()
	inner := &bothLifecycleToolSet{runnableToolSet: runnableToolSet{run: func(context.Context) {
		t.Error("Run must not bypass Startable")
	}}}
	s := tools.NewStartable(tools.WithName(inner, "worker"))
	require.NoError(t, s.Start(t.Context()))
	require.NoError(t, s.Start(t.Context()))
	require.NoError(t, s.Stop(t.Context()))
	assert.Equal(t, 1, inner.starts)
	assert.Equal(t, 1, inner.stops)
}

func TestRunnablePendingStopHandoff(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		started := make(chan struct{})
		finished := make(chan struct{})
		s := tools.NewStartable(&runnableToolSet{run: func(ctx context.Context) {
			defer close(finished)
			close(started)
			<-ctx.Done()
		}})
		require.NoError(t, s.Start(t.Context()))
		<-started
		s.ExportedPublishStopRequest(t.Context())
		assert.False(t, s.TryIsStarted())
		<-finished
		require.NoError(t, s.StopIfStarted(t.Context()))
	})
}

type restartableRunnable struct {
	runnableToolSet

	restarts atomic.Int32
}

func (r *restartableRunnable) Restart(context.Context) error {
	r.restarts.Add(1)
	return errors.New("restart failed")
}

func TestRunnableRejectsUnownedRestart(t *testing.T) {
	t.Parallel()
	for _, pending := range []bool{false, true} {
		t.Run(strconv.FormatBool(pending), func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				started := make(chan context.Context, 2)
				inner := &restartableRunnable{runnableToolSet: runnableToolSet{run: func(ctx context.Context) {
					started <- ctx
					<-ctx.Done()
				}}}
				s := tools.NewStartable(inner)
				t.Cleanup(func() { require.NoError(t, s.Stop(t.Context())) })
				require.NoError(t, s.Start(t.Context()))
				runCtx := <-started
				assert.False(t, s.CanRestart())
				require.ErrorContains(t, s.RestartIfSupported(t.Context()), "does not support restart")
				assert.Zero(t, inner.restarts.Load())
				assert.True(t, s.IsStarted(), "rejected restart must not lose the running worker")
				if pending {
					s.ExportedPublishStopRequest(t.Context())
					assert.False(t, s.TryIsStarted())
				} else {
					require.NoError(t, s.StopIfStarted(t.Context()))
				}
				require.ErrorIs(t, runCtx.Err(), context.Canceled)
				require.NoError(t, s.Start(t.Context()))
				runCtx = <-started
				require.NoError(t, runCtx.Err())
			})
		})
	}
}

func TestRunnableNestedRestartOwnership(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		started := make(chan context.Context, 2)
		release := make(chan struct{})
		var runs atomic.Int32
		inner := &restartableRunnable{runnableToolSet: runnableToolSet{run: func(ctx context.Context) {
			generation := runs.Add(1)
			started <- ctx
			if generation == 1 {
				select {
				case <-release:
				case <-ctx.Done():
				}
				return
			}
			<-ctx.Done()
		}}}
		owner := tools.NewStartable(inner)
		middle := tools.NewStartable(tools.WithName(owner, "worker"))
		outer := tools.NewStartable(middle)
		t.Cleanup(func() { require.NoError(t, outer.Stop(t.Context())) })
		require.NoError(t, outer.Start(t.Context()))
		first := <-started
		assert.False(t, owner.CanRestart())
		assert.False(t, outer.CanRestart())
		require.ErrorContains(t, outer.RestartIfSupported(t.Context()), "does not support restart")
		assert.True(t, outer.IsStarted())
		assert.Zero(t, inner.restarts.Load())
		close(release)
		synctest.Wait()
		require.ErrorIs(t, first.Err(), context.Canceled)
		assert.False(t, middle.IsStarted())
		assert.False(t, outer.IsStarted())
		assert.False(t, outer.TryIsHealthy())
		require.NoError(t, outer.Start(t.Context()))
		second := <-started
		assert.True(t, owner.IsStarted())
		assert.True(t, outer.IsStarted())
		assert.EqualValues(t, 2, runs.Load())
		assert.Zero(t, inner.restarts.Load())
		require.NoError(t, outer.StopIfStarted(t.Context()))
		require.ErrorIs(t, second.Err(), context.Canceled)
	})
}

type blockingRestartToolSet struct {
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (*blockingRestartToolSet) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }
func (*blockingRestartToolSet) Start(context.Context) error                 { return nil }
func (*blockingRestartToolSet) Stop(context.Context) error                  { return nil }
func (r *blockingRestartToolSet) Restart(context.Context) error {
	r.calls.Add(1)
	r.entered <- struct{}{}
	<-r.release
	return nil
}

func TestNestedLifecycleBusyIsNotLoss(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		inner := &blockingRestartToolSet{entered: make(chan struct{}, 2), release: make(chan struct{})}
		owner := tools.NewStartable(inner)
		middle := tools.NewStartable(owner)
		outer := tools.NewStartable(middle)
		require.NoError(t, outer.Start(t.Context()))
		restarted := make(chan error, 1)
		go func() { restarted <- owner.RestartIfSupported(t.Context()) }()
		<-inner.entered
		started := make(chan error, 1)
		go func() { started <- outer.Start(t.Context()) }()
		synctest.Wait()
		assert.False(t, middle.TryIsHealthy(), "nonblocking probes must not wait for the inner lock")
		close(inner.release)
		require.NoError(t, <-restarted)
		require.NoError(t, <-started)
		assert.EqualValues(t, 1, inner.calls.Load(), "ordinary Start must not queue a redundant restart")
		require.NoError(t, outer.StopIfStarted(t.Context()))
	})
}
