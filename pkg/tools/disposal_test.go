package tools_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/codemode"
)

type disposalToolSet struct {
	stops, closes atomic.Int32
	live          atomic.Bool
	list          func(context.Context) ([]tools.Tool, error)
	started       chan struct{}
	release       chan struct{}
	stopErr       error
}

func (s *disposalToolSet) Tools(ctx context.Context) ([]tools.Tool, error) {
	if s.list != nil {
		return s.list(ctx)
	}
	return nil, nil
}

func (s *disposalToolSet) Start(context.Context) error {
	if s.started != nil {
		close(s.started)
		<-s.release
	}
	return nil
}

func (s *disposalToolSet) Stop(context.Context) error {
	s.stops.Add(1)
	s.live.Store(false)
	return s.stopErr
}

func (s *disposalToolSet) Close() error {
	s.closes.Add(1)
	return nil
}

func TestDisposeNeverStartedSharedComposite(t *testing.T) {
	t.Parallel()
	raw := &disposalToolSet{}
	shared := tools.NewStartable(raw)
	composite := tools.NewStartable(codemode.Wrap(shared, shared))
	require.NoError(t, tools.Dispose(t.Context(), composite, shared))
	require.NoError(t, tools.Dispose(t.Context(), composite, shared))
	assert.EqualValues(t, 1, raw.stops.Load())
	assert.EqualValues(t, 1, raw.closes.Load())
	require.ErrorContains(t, shared.Start(t.Context()), "disposed")
	require.ErrorContains(t, composite.Start(t.Context()), "disposed")
}

func TestDisposeAfterStopDoesNotStopTwice(t *testing.T) {
	t.Parallel()
	raw := &disposalToolSet{}
	owner := tools.NewStartable(raw)
	require.NoError(t, owner.Start(t.Context()))
	require.NoError(t, owner.Stop(t.Context()))
	require.NoError(t, owner.Dispose(t.Context()))
	assert.EqualValues(t, 1, raw.stops.Load())
	assert.EqualValues(t, 1, raw.closes.Load())
}

func TestDisposeAggregatesErrorsAndCloses(t *testing.T) {
	t.Parallel()
	failure := errors.New("stop failed")
	first := &disposalToolSet{stopErr: failure}
	last := &disposalToolSet{}
	require.ErrorIs(t, tools.Dispose(t.Context(), tools.NewStartable(first), tools.NewStartable(last)), failure)
	assert.EqualValues(t, 1, first.closes.Load())
	assert.EqualValues(t, 1, last.closes.Load())
}

func TestDisposeWaitsForEverySharedOwner(t *testing.T) {
	t.Parallel()
	raw := &disposalToolSet{started: make(chan struct{}), release: make(chan struct{})}
	idle, running := tools.NewStartable(raw), tools.NewStartable(raw)
	started := make(chan error, 1)
	go func() { started <- running.Start(t.Context()) }()
	<-raw.started
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, tools.Dispose(ctx, idle, running), context.Canceled)
	assert.Zero(t, raw.stops.Load())
	assert.Zero(t, raw.closes.Load())
	close(raw.release)
	require.NoError(t, <-started)
	require.Eventually(t, func() bool { return raw.closes.Load() == 1 }, time.Second, time.Millisecond)
	require.NoError(t, tools.Dispose(t.Context(), idle, running))
	assert.EqualValues(t, 1, raw.stops.Load())
	assert.EqualValues(t, 1, raw.closes.Load())
}

func TestDisposeStopsRestartedInnerOwner(t *testing.T) {
	t.Parallel()
	raw := &disposalToolSet{}
	inner := tools.NewStartable(raw)
	outer := tools.NewStartable(inner)
	require.NoError(t, outer.Start(t.Context()))
	require.NoError(t, outer.Stop(t.Context()))
	require.NoError(t, inner.Start(t.Context()))
	require.NoError(t, tools.Dispose(t.Context(), outer, inner))
	assert.EqualValues(t, 2, raw.stops.Load())
	assert.EqualValues(t, 1, raw.closes.Load())
}

func TestDisposeCleansIndependentRootsWhileStartIsStuck(t *testing.T) {
	t.Parallel()
	wedged := &disposalToolSet{started: make(chan struct{}), release: make(chan struct{})}
	release := sync.OnceFunc(func() { close(wedged.release) })
	defer release()
	idle, running := tools.NewStartable(wedged), tools.NewStartable(wedged)
	ctx := t.Context()
	started := make(chan error, 1)
	go func() { started <- running.Start(ctx) }()
	<-wedged.started

	independent := &disposalToolSet{}
	owner := tools.NewStartable(independent)
	disposeCtx, cancel := context.WithCancel(ctx)
	cancel()
	require.ErrorIs(t, tools.Dispose(disposeCtx, idle, running, owner), context.Canceled)
	assert.EqualValues(t, 1, independent.stops.Load())
	assert.EqualValues(t, 1, independent.closes.Load())
	assert.Zero(t, wedged.closes.Load())
	require.ErrorContains(t, idle.Start(ctx), "disposed")
	release()
	require.NoError(t, <-started)
	require.Eventually(t, func() bool { return wedged.closes.Load() == 1 }, time.Second, time.Millisecond)
	require.NoError(t, tools.Dispose(ctx, running, idle, owner))
	assert.EqualValues(t, 1, wedged.stops.Load())
	assert.EqualValues(t, 1, wedged.closes.Load())
	assert.EqualValues(t, 1, independent.closes.Load())
}

func TestDisposeWaitsForLateListingAndRejectsNewListings(t *testing.T) {
	t.Parallel()
	for _, stopped := range []bool{false, true} {
		t.Run(map[bool]string{false: "never started", true: "after stop"}[stopped], func(t *testing.T) {
			t.Parallel()
			entered, resume := make(chan struct{}), make(chan struct{})
			release := sync.OnceFunc(func() { close(resume) })
			defer release()
			var listings atomic.Int32
			raw := &disposalToolSet{}
			raw.list = func(context.Context) ([]tools.Tool, error) {
				if listings.Add(1) == 1 {
					close(entered)
					<-resume
				}
				raw.live.Store(true)
				return nil, nil
			}
			owner := tools.NewStartable(raw)
			if stopped {
				require.NoError(t, owner.Start(t.Context()))
				require.NoError(t, owner.Stop(t.Context()))
			}
			previousStops := raw.stops.Load()
			ctx := t.Context()
			listed := make(chan error, 1)
			go func() { _, err := owner.Tools(ctx); listed <- err }()
			<-entered

			independent := &disposalToolSet{}
			disposeCtx, cancel := context.WithCancel(ctx)
			cancel()
			require.ErrorIs(t, tools.Dispose(disposeCtx, owner, tools.NewStartable(independent)), context.Canceled)
			assert.Zero(t, raw.closes.Load(), "listing may still create a resource")
			assert.Equal(t, previousStops, raw.stops.Load())
			assert.EqualValues(t, 1, independent.closes.Load())
			_, err := owner.Tools(ctx)
			require.ErrorContains(t, err, "disposed")
			assert.EqualValues(t, 1, listings.Load())

			release()
			require.NoError(t, <-listed)
			require.Eventually(t, func() bool { return raw.closes.Load() == 1 }, time.Second, time.Millisecond)
			require.NoError(t, owner.Dispose(ctx))
			assert.Equal(t, previousStops+1, raw.stops.Load())
			assert.False(t, raw.live.Load(), "late listing startup must be stopped")
			_, err = owner.Tools(ctx)
			require.ErrorContains(t, err, "disposed")
			assert.EqualValues(t, 1, listings.Load())
		})
	}
}

func TestDisposeJoinsOverlappingRootsTransitively(t *testing.T) {
	t.Parallel()
	shared := &disposalToolSet{started: make(chan struct{}), release: make(chan struct{})}
	release := sync.OnceFunc(func() { close(shared.release) })
	defer release()
	idle, running := tools.NewStartable(shared), tools.NewStartable(shared)
	other := &disposalToolSet{}
	otherOwner := tools.NewStartable(other)
	bridge := tools.NewStartable(codemode.Wrap(idle, otherOwner))
	ctx := t.Context()
	started := make(chan error, 1)
	go func() { started <- running.Start(ctx) }()
	<-shared.started

	disposeCtx, cancel := context.WithCancel(ctx)
	cancel()
	require.ErrorIs(t, tools.Dispose(disposeCtx, running, otherOwner, bridge), context.Canceled)
	assert.Zero(t, shared.closes.Load())
	assert.Zero(t, other.closes.Load(), "bridge owns both resources and must drain first")
	release()
	require.NoError(t, <-started)
	require.Eventually(t, func() bool {
		return shared.closes.Load() == 1 && other.closes.Load() == 1
	}, time.Second, time.Millisecond)
	require.NoError(t, tools.Dispose(ctx, running, otherOwner, bridge))
	assert.EqualValues(t, 1, shared.stops.Load())
	assert.EqualValues(t, 1, other.stops.Load())
}

func TestDisposeDrainsEveryOutstandingListing(t *testing.T) {
	t.Parallel()
	entered := make(chan int, 2)
	resume := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
	release := sync.OnceFunc(func() {
		close(resume[0])
		close(resume[1])
	})
	defer release()
	var listings atomic.Int32
	raw := &disposalToolSet{list: func(context.Context) ([]tools.Tool, error) {
		i := int(listings.Add(1)) - 1
		entered <- i
		<-resume[i]
		return nil, nil
	}}
	owner := tools.NewStartable(raw)
	ctx := t.Context()
	listed := make(chan error, 2)
	for range 2 {
		go func() { _, err := owner.Tools(ctx); listed <- err }()
	}
	<-entered
	<-entered

	disposed := make(chan error, 1)
	go func() { disposed <- owner.Dispose(ctx) }()
	require.Eventually(t, func() bool { return !owner.TryIsAvailable() }, time.Second, time.Millisecond)
	resume[0] <- struct{}{}
	require.NoError(t, <-listed)
	assert.Zero(t, raw.closes.Load(), "the second listing must also drain")
	release()
	require.NoError(t, <-listed)
	require.NoError(t, <-disposed)
	assert.EqualValues(t, 1, raw.stops.Load())
	assert.EqualValues(t, 1, raw.closes.Load())
}

func TestDisposeStopsSharedLeafRegardlessOfOwnerOrder(t *testing.T) {
	t.Parallel()
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse=%t", reverse), func(t *testing.T) {
			t.Parallel()
			raw := &disposalToolSet{}
			stopped, running := tools.NewStartable(raw), tools.NewStartable(raw)
			require.NoError(t, stopped.Start(t.Context()))
			require.NoError(t, stopped.Stop(t.Context()))
			require.NoError(t, running.Start(t.Context()))
			raw.live.Store(true)
			owners := []tools.ToolSet{stopped, running}
			if reverse {
				slices.Reverse(owners)
			}
			require.NoError(t, tools.Dispose(t.Context(), owners...))
			assert.EqualValues(t, 2, raw.stops.Load())
			assert.False(t, raw.live.Load())
			assert.EqualValues(t, 1, raw.closes.Load())
			require.NoError(t, tools.Dispose(t.Context(), owners...))
			assert.EqualValues(t, 2, raw.stops.Load())
			assert.EqualValues(t, 1, raw.closes.Load())
		})
	}
}

type disposalBothEdges struct {
	inner    tools.ToolSet
	children []tools.ToolSet
}

func (*disposalBothEdges) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }
func (s *disposalBothEdges) Unwrap() tools.ToolSet                     { return s.inner }
func (s *disposalBothEdges) Children() []tools.ToolSet                 { return s.children }

func TestDisposeTraversesBothEdgeTypes(t *testing.T) {
	t.Parallel()
	decorated, child := &disposalToolSet{}, &disposalToolSet{}
	shared := tools.NewStartable(decorated)
	root := &disposalBothEdges{inner: shared, children: []tools.ToolSet{shared, tools.NewStartable(child)}}
	require.NoError(t, tools.Dispose(t.Context(), root))
	for _, raw := range []*disposalToolSet{decorated, child} {
		assert.EqualValues(t, 1, raw.stops.Load())
		assert.EqualValues(t, 1, raw.closes.Load())
	}
}
