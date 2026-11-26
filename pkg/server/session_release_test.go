package server

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// stopTrackingToolSet counts Start and Stop calls, standing in for an MCP toolset.
type stopTrackingToolSet struct {
	started atomic.Int32
	stopped atomic.Int32
}

func (s *stopTrackingToolSet) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }
func (s *stopTrackingToolSet) Start(context.Context) error                 { s.started.Add(1); return nil }
func (s *stopTrackingToolSet) Stop(context.Context) error                  { s.stopped.Add(1); return nil }

// closeTrackingRuntime records Close and tolerates the sink resets finishSessionDeletion
// performs on server-owned runtimes.
type closeTrackingRuntime struct {
	fakeRuntime

	closed atomic.Int32
}

func (r *closeTrackingRuntime) OnElicitationRequest(func(runtime.Event)) {}
func (r *closeTrackingRuntime) OnBackgroundEvent(func(runtime.Event))    {}
func (r *closeTrackingRuntime) Close() error                             { r.closed.Add(1); return nil }

func newStartedTeam(t *testing.T) (*team.Team, *stopTrackingToolSet) {
	t.Helper()

	ts := &stopTrackingToolSet{}
	root := agent.New("root", "root", agent.WithToolSets(ts))
	tm := team.New(team.WithAgents(root))
	// Collecting the agent's tools starts its toolsets, as a run does.
	_, err := root.Tools(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 1, ts.started.Load())
	return tm, ts
}

// Deleting a session must close its runtime and stop its team's toolsets; nothing else does.
func TestDeleteSession_ReleasesServerOwnedRuntimeAndStopsTeamToolSets(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})

	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))

	tm, ts := newStartedTeam(t)
	rt := &closeTrackingRuntime{}
	sm.runtimeSessions.Store(sess.ID, &activeRuntimes{
		runtime: rt,
		team:    tm,
		cancel:  func() {},
		session: sess,
	})

	require.NoError(t, sm.DeleteSession(ctx, sess.ID))

	require.Eventually(t, func() bool {
		return ts.stopped.Load() == 1 && rt.closed.Load() == 1
	}, 5*time.Second, 10*time.Millisecond, "deleting a session must stop its team's toolsets and close its runtime")
}

// Attached runtimes (done != nil) belong to their embedder.
func TestDeleteSession_LeavesAttachedRuntimeAlone(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})

	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))

	tm, ts := newStartedTeam(t)
	rt := &closeTrackingRuntime{}
	done := make(chan struct{})
	sm.runtimeSessions.Store(sess.ID, &activeRuntimes{
		runtime: rt,
		team:    tm,
		done:    done,
		cancel:  func() {},
		session: sess,
	})

	require.NoError(t, sm.DeleteSession(ctx, sess.ID))

	assert.Never(t, func() bool {
		return ts.stopped.Load() != 0 || rt.closed.Load() != 0
	}, 300*time.Millisecond, 10*time.Millisecond, "an embedder-owned runtime must not be closed by the session manager")
}

// The real AttachRuntime path must be left alone too, not only a hand-built entry.
func TestDeleteSession_LeavesRuntimeFromAttachRuntimeAlone(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})

	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))

	rt := &closeTrackingRuntime{}
	sm.AttachRuntime(ctx, sess.ID, rt, sess)

	require.NoError(t, sm.DeleteSession(ctx, sess.ID))

	assert.Never(t, func() bool { return rt.closed.Load() != 0 },
		300*time.Millisecond, 10*time.Millisecond, "an attached runtime must not be closed by the session manager")
}

// hangingCloseRuntime blocks in Close until unblock is closed, like a LocalRuntime waiting on a
// background agent that never exits, or panics if panics is set.
type hangingCloseRuntime struct {
	closeTrackingRuntime

	unblock chan struct{}
	panics  bool
}

func (r *hangingCloseRuntime) Close() error {
	r.closed.Add(1)
	if r.panics {
		panic("close exploded")
	}
	<-r.unblock
	return nil
}

// A Close that hangs or panics must not prevent the toolsets from being stopped.
func TestReleaseRuntimeResources_StopsToolSetsWhenCloseHangs(t *testing.T) {
	t.Parallel()

	tm, ts := newStartedTeam(t)
	rt := &hangingCloseRuntime{unblock: make(chan struct{})}
	t.Cleanup(func() { close(rt.unblock) })

	releaseRuntimeResourcesWithin(t.Context(), "s", rt, tm, 50*time.Millisecond)

	assert.EqualValues(t, 1, rt.closed.Load())
	assert.EqualValues(t, 1, ts.stopped.Load(), "toolsets must be stopped even though Close never returned")
}

func TestReleaseRuntimeResources_StopsToolSetsWhenClosePanics(t *testing.T) {
	t.Parallel()

	tm, ts := newStartedTeam(t)
	rt := &hangingCloseRuntime{panics: true}

	releaseRuntimeResourcesWithin(t.Context(), "s", rt, tm, time.Second)

	assert.EqualValues(t, 1, ts.stopped.Load(), "toolsets must be stopped even though Close panicked")
}

// A nil team is skipped and the runtime is still closed.
func TestReleaseRuntimeResources_NilTeam(t *testing.T) {
	t.Parallel()

	rt := &closeTrackingRuntime{}
	releaseRuntimeResources(t.Context(), "s", rt, nil)
	assert.EqualValues(t, 1, rt.closed.Load())
}

// A running stream must end before the runtime is released, so toolsets are not stopped mid-turn.
func TestDeleteSession_WaitsForInFlightStreamBeforeRelease(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})

	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))

	tm, ts := newStartedTeam(t)
	rt := &closeTrackingRuntime{}
	rs := &activeRuntimes{runtime: rt, team: tm, cancel: func() {}, session: sess}
	sm.runtimeSessions.Store(sess.ID, rs)

	rs.streaming.Lock() // a turn is in flight
	require.NoError(t, sm.DeleteSession(ctx, sess.ID))

	assert.Never(t, func() bool {
		return ts.stopped.Load() != 0 || rt.closed.Load() != 0
	}, 500*time.Millisecond, 10*time.Millisecond, "the runtime must not be released while its stream is running")

	rs.streaming.Unlock() // the turn ends

	require.Eventually(t, func() bool {
		return ts.stopped.Load() == 1 && rt.closed.Load() == 1
	}, 5*time.Second, 10*time.Millisecond, "the runtime must be released once the stream has ended")
}

// Batch deletion (trackStopped=false) must release too.
func TestBatchDeleteSessions_ReleasesServerOwnedRuntimes(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})

	var ids []string
	var toolsets []*stopTrackingToolSet
	var runtimes []*closeTrackingRuntime
	for range 3 {
		sess := session.New()
		require.NoError(t, store.AddSession(ctx, sess))
		tm, ts := newStartedTeam(t)
		rt := &closeTrackingRuntime{}
		sm.runtimeSessions.Store(sess.ID, &activeRuntimes{runtime: rt, team: tm, cancel: func() {}, session: sess})
		ids = append(ids, sess.ID)
		toolsets = append(toolsets, ts)
		runtimes = append(runtimes, rt)
	}

	deleted, failed := sm.BatchDeleteSessions(ctx, ids)
	require.Equal(t, 3, deleted)
	require.Empty(t, failed)

	require.Eventually(t, func() bool {
		for i := range ids {
			if toolsets[i].stopped.Load() != 1 || runtimes[i].closed.Load() != 1 {
				return false
			}
		}
		return true
	}, 5*time.Second, 10*time.Millisecond, "every batch-deleted session must be released")
}

// RunSession must record the team it built, or deletion has nothing to stop. It goes through
// RunSession rather than building activeRuntimes by hand so that dropping the assignment fails.
func TestRunSession_RecordsTeamForRelease(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	sources := config.Sources{"agent.yaml": config.NewBytesSource("agent.yaml", []byte(plainConfig))}
	sm := NewSessionManager(ctx, sources, store, 0, &config.RuntimeConfig{})

	rt := &closeTrackingRuntime{}
	var builtTeam *team.Team
	sm.newRuntime = func(_ context.Context, tm *team.Team, _ ...runtime.Opt) (runtime.Runtime, error) {
		builtTeam = tm
		return rt, nil
	}

	sess, err := sm.CreateSession(ctx, session.New())
	require.NoError(t, err)

	events, err := sm.RunSession(ctx, sess.ID, "agent.yaml", "root", []api.Message{{Content: "hi"}}, "")
	require.NoError(t, err)
	for range events {
	}

	rs, ok := sm.runtimeSessions.Load(sess.ID)
	require.True(t, ok)
	require.NotNil(t, builtTeam)
	assert.Same(t, builtTeam, rs.team, "RunSession must record the team it built so deletion can stop its toolsets")

	require.NoError(t, sm.DeleteSession(ctx, sess.ID))
	require.Eventually(t, func() bool { return rt.closed.Load() == 1 }, 5*time.Second, 10*time.Millisecond)
}
