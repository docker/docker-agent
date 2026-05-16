package server

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/concurrent"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
	"github.com/docker/docker-agent/pkg/tools"
)

// fakeRuntime is a minimal Runtime that records concurrent RunStream calls.
type fakeRuntime struct {
	runtime.Runtime

	concurrentStreams atomic.Int32
	maxConcurrent     atomic.Int32
	streamDelay       time.Duration
}

func (f *fakeRuntime) RunStream(_ context.Context, _ *session.Session) <-chan runtime.Event {
	cur := f.concurrentStreams.Add(1)
	for {
		old := f.maxConcurrent.Load()
		if cur <= old || f.maxConcurrent.CompareAndSwap(old, cur) {
			break
		}
	}

	ch := make(chan runtime.Event)
	go func() {
		time.Sleep(f.streamDelay)
		f.concurrentStreams.Add(-1)
		close(ch)
	}()
	return ch
}

func (f *fakeRuntime) Resume(_ context.Context, _ runtime.ResumeRequest) {}

func (f *fakeRuntime) Steer(_ runtime.QueuedMessage) error { return nil }

func (f *fakeRuntime) FollowUp(_ runtime.QueuedMessage) error { return nil }

func (f *fakeRuntime) ResumeElicitation(_ context.Context, _ tools.ElicitationAction, _ map[string]any) error {
	return nil
}

func (f *fakeRuntime) CurrentAgentName() string { return "root" }

func newTestSessionManager(t *testing.T, sess *session.Session, fake *fakeRuntime) *SessionManager {
	t.Helper()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	require.NoError(t, store.AddSession(ctx, sess))

	sm := &SessionManager{
		runtimeSessions: concurrent.NewMap[string, *activeRuntimes](),
		deletedSessions: concurrent.NewMap[string, *activeRuntimes](),
		sessionStore:    store,
		Sources:         config.Sources{},
		runConfig:       &config.RuntimeConfig{},
		sessionReady:    make(chan struct{}),
	}

	// Pre-register a runtime for this session so RunSession skips agent loading.
	sm.runtimeSessions.Store(sess.ID, &activeRuntimes{
		runtime:  fake,
		session:  sess,
		titleGen: (*sessiontitle.Generator)(nil),
	})

	return sm
}

// TestAttachRuntime_RegistersRuntimeForExternalDriver verifies that a
// pre-built runtime is reachable through the manager API after AttachRuntime.
// This is what lets the TUI hand its in-process runtime to an HTTP server.
func TestAttachRuntime_RegistersRuntimeForExternalDriver(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})
	fake := &fakeRuntime{streamDelay: 10 * time.Millisecond}
	sm.AttachRuntime(sess.ID, fake, sess)

	// Steer routes through the attached runtime, not a freshly built one.
	require.NoError(t, sm.SteerSession(ctx, sess.ID, []api.Message{{Content: "hi"}}))
}

// TestRunSession_ConcurrentRequestReturnsErrSessionBusy verifies that a
// second RunSession call on a session that is already streaming returns
// ErrSessionBusy instead of silently interleaving messages.
func TestRunSession_ConcurrentRequestReturnsErrSessionBusy(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sess := session.New()
	fake := &fakeRuntime{streamDelay: 500 * time.Millisecond}
	sm := newTestSessionManager(t, sess, fake)

	// Start the first stream.
	ch1, err := sm.RunSession(ctx, sess.ID, "agent", "root", []api.Message{
		{Content: "first"},
	})
	require.NoError(t, err)

	// Give the goroutine a moment to acquire the streaming lock.
	time.Sleep(50 * time.Millisecond)

	// The second request should fail immediately with ErrSessionBusy.
	_, err = sm.RunSession(ctx, sess.ID, "agent", "root", []api.Message{
		{Content: "second"},
	})
	require.ErrorIs(t, err, ErrSessionBusy)

	// Drain first stream to let it complete.
	for range ch1 {
	}

	// After the first stream finishes, a new request should succeed.
	ch3, err := sm.RunSession(ctx, sess.ID, "agent", "root", []api.Message{
		{Content: "third"},
	})
	require.NoError(t, err)
	for range ch3 {
	}
}

// TestRunSession_MessagesNotAddedWhenBusy verifies that when a session
// is busy, the rejected request does not mutate the session's messages.
func TestRunSession_MessagesNotAddedWhenBusy(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sess := session.New()
	fake := &fakeRuntime{streamDelay: 500 * time.Millisecond}
	sm := newTestSessionManager(t, sess, fake)

	ch1, err := sm.RunSession(ctx, sess.ID, "agent", "root", []api.Message{
		{Content: "first"},
	})
	require.NoError(t, err)

	time.Sleep(50 * time.Millisecond)

	msgCountBefore := len(sess.GetAllMessages())

	_, err = sm.RunSession(ctx, sess.ID, "agent", "root", []api.Message{
		{Content: "should not be added"},
	})
	require.ErrorIs(t, err, ErrSessionBusy)

	// Messages should not have been added.
	assert.Len(t, sess.GetAllMessages(), msgCountBefore)

	for range ch1 {
	}
}

// TestRunSession_SequentialRequestsSucceed verifies that sequential
// (non-overlapping) requests on the same session work normally.
func TestRunSession_SequentialRequestsSucceed(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sess := session.New()
	fake := &fakeRuntime{streamDelay: 10 * time.Millisecond}
	sm := newTestSessionManager(t, sess, fake)

	for range 3 {
		ch, err := sm.RunSession(ctx, sess.ID, "agent", "root", []api.Message{
			{Content: "hello"},
		})
		require.NoError(t, err)
		for range ch {
		}
	}

	assert.Equal(t, int32(1), fake.maxConcurrent.Load())
}

// TestRunSession_DifferentSessionsConcurrently verifies that concurrent
// requests on *different* sessions are not blocked by each other.
func TestRunSession_DifferentSessionsConcurrently(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	fake1 := &fakeRuntime{streamDelay: 200 * time.Millisecond}
	fake2 := &fakeRuntime{streamDelay: 200 * time.Millisecond}

	sess1 := session.New()
	sess2 := session.New()
	require.NoError(t, store.AddSession(ctx, sess1))
	require.NoError(t, store.AddSession(ctx, sess2))

	sm := &SessionManager{
		runtimeSessions: concurrent.NewMap[string, *activeRuntimes](),
		deletedSessions: concurrent.NewMap[string, *activeRuntimes](),
		sessionStore:    store,
		Sources:         config.Sources{},
		runConfig:       &config.RuntimeConfig{},
		sessionReady:    make(chan struct{}),
	}

	sm.runtimeSessions.Store(sess1.ID, &activeRuntimes{
		runtime: fake1, session: sess1, titleGen: (*sessiontitle.Generator)(nil),
	})
	sm.runtimeSessions.Store(sess2.ID, &activeRuntimes{
		runtime: fake2, session: sess2, titleGen: (*sessiontitle.Generator)(nil),
	})

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		ch, err := sm.RunSession(ctx, sess1.ID, "agent", "root", []api.Message{{Content: "a"}})
		assert.NoError(t, err)
		for range ch {
		}
	}()

	go func() {
		defer wg.Done()
		ch, err := sm.RunSession(ctx, sess2.ID, "agent", "root", []api.Message{{Content: "b"}})
		assert.NoError(t, err)
		for range ch {
		}
	}()

	wg.Wait()

	// Both sessions should have streamed (1 each).
	assert.Equal(t, int32(1), fake1.maxConcurrent.Load())
	assert.Equal(t, int32(1), fake2.maxConcurrent.Load())
}
