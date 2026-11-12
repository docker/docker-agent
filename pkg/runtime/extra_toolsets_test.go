package runtime

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/scheduler"
)

type extraRecallRuntime struct {
	tools.NopRuntime

	messages chan string
}

func (*extraRecallRuntime) Supports(c tools.Capability) bool { return c == tools.CapabilityRecall }
func (r *extraRecallRuntime) Recall(ctx context.Context, msg string) error {
	select {
	case r.messages <- msg:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestExtraSchedulerLifecycle(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		root := agent.New("root", "test", agent.WithModel(&mockProvider{id: "test/mock-model", stream: &mockStream{}}))
		r, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithModelStore(mockModelStore{}))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, r.Close()) })
		raw := scheduler.New()
		first := session.New(session.WithExtraToolSets([]tools.ToolSet{raw}))
		second := session.New(session.WithExtraToolSets([]tools.ToolSet{tools.WithName(raw, "schedule")}))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var listed []tools.Tool
		for range 3 {
			listed = r.skillSubSessionTools(ctx, first, root, nil)
			require.Len(t, listed, 3)
			require.Len(t, r.skillSubSessionTools(ctx, second, root, nil), 3)
		}
		assert.Len(t, r.extraToolsetRuns.entries, 1, "decorated references share one run")
		host := &extraRecallRuntime{messages: make(chan string, 2)}
		result, err := listed[0].Handler(ctx, tools.ToolCall{Function: tools.FunctionCall{
			Name: scheduler.ToolNameCreateSchedule, Arguments: `{"prompt":"check","when":"every:1m"}`,
		}}, host)
		require.NoError(t, err)
		require.False(t, result.IsError)
		cancel()
		synctest.Sleep(time.Minute)
		assert.Len(t, host.messages, 1)
		<-host.messages
		require.NoError(t, r.Close())
		synctest.Sleep(time.Minute)
		assert.Empty(t, host.messages)
		assert.Empty(t, r.skillSubSessionTools(t.Context(), first, root, nil), "closed runtime cannot restart raw workers")
	})
}

type extraWorker struct {
	run func(context.Context)
}

func (*extraWorker) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }
func (w *extraWorker) Run(ctx context.Context)                   { w.run(ctx) }

func TestExtraRunnableBorrowedOwnership(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		started := make(chan context.Context, 1)
		inner := &extraWorker{run: func(ctx context.Context) { started <- ctx; <-ctx.Done() }}
		owner := tools.NewStartable(inner)
		t.Cleanup(func() { require.NoError(t, owner.Stop(t.Context())) })
		var runs extraToolsetRuns
		sess := session.New()
		require.NoError(t, runs.start(t.Context(), sess, 0, tools.WithName(owner, "worker")))
		runCtx := <-started
		require.NoError(t, runs.close(t.Context()))
		require.NoError(t, runCtx.Err(), "runtime must not stop team/embedder-owned workers")
		require.NoError(t, owner.Stop(t.Context()))
		require.ErrorIs(t, runCtx.Err(), context.Canceled)
	})
}

func TestExtraRunnableCloseRacesStarts(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var active, launches atomic.Int32
		inner := &extraWorker{run: func(ctx context.Context) {
			active.Add(1)
			launches.Add(1)
			defer active.Add(-1)
			<-ctx.Done()
		}}
		var runs extraToolsetRuns
		sess := session.New()
		require.NoError(t, runs.start(t.Context(), sess, 0, inner))
		synctest.Wait()
		var callers sync.WaitGroup
		for range 20 {
			callers.Go(func() { _ = runs.start(t.Context(), sess, 0, inner) })
			callers.Go(func() { assert.NoError(t, runs.close(t.Context())) })
		}
		callers.Wait()
		assert.Zero(t, active.Load())
		assert.EqualValues(t, 1, launches.Load())
		require.ErrorContains(t, runs.start(t.Context(), sess, 0, inner), "closed")
	})
}

type noncomparableExtra struct {
	start []func(context.Context)
}

func (w noncomparableExtra) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }
func (w noncomparableExtra) Run(ctx context.Context)                     { w.start[0](ctx) }

func TestExtraRunnableNoncomparable(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var launches atomic.Int32
		inner := noncomparableExtra{start: []func(context.Context){func(ctx context.Context) {
			launches.Add(1)
			<-ctx.Done()
		}}}
		var runs extraToolsetRuns
		sess := session.New()
		for range 3 {
			require.NoError(t, runs.start(t.Context(), sess, 0, inner))
		}
		synctest.Wait()
		assert.EqualValues(t, 1, launches.Load())
		require.NoError(t, runs.close(t.Context()))
	})
}
