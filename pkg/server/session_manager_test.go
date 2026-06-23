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
	"github.com/docker/docker-agent/pkg/chat"
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

// SupportsModelSwitching reports false by default. Tests that exercise
// the /models endpoints embed fakeRuntime and override this.
func (f *fakeRuntime) SupportsModelSwitching() bool { return false }

func newTestSessionManager(t *testing.T, sess *session.Session, fake *fakeRuntime) *SessionManager {
	t.Helper()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	require.NoError(t, store.AddSession(ctx, sess))

	sm := &SessionManager{
		runtimeSessions:   concurrent.NewMap[string, *activeRuntimes](),
		deletedSessions:   concurrent.NewMap[string, *activeRuntimes](),
		followUpInjectors: concurrent.NewMap[string, FollowUpInjector](),
		followUpKeys:      concurrent.NewMap[string, *idempotencyCache](),
		sessionStore:      store,
		Sources:           config.Sources{},
		runConfig:         &config.RuntimeConfig{},
		sessionReady:      make(chan struct{}),
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
	}, "")
	require.NoError(t, err)

	// Give the goroutine a moment to acquire the streaming lock.
	time.Sleep(50 * time.Millisecond)

	// The second request should fail immediately with ErrSessionBusy.
	_, err = sm.RunSession(ctx, sess.ID, "agent", "root", []api.Message{
		{Content: "second"},
	}, "")
	require.ErrorIs(t, err, ErrSessionBusy)

	// Drain first stream to let it complete.
	for range ch1 {
	}

	// After the first stream finishes, a new request should succeed.
	ch3, err := sm.RunSession(ctx, sess.ID, "agent", "root", []api.Message{
		{Content: "third"},
	}, "")
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
	}, "")
	require.NoError(t, err)

	time.Sleep(50 * time.Millisecond)

	msgCountBefore := len(sess.GetAllMessages())

	_, err = sm.RunSession(ctx, sess.ID, "agent", "root", []api.Message{
		{Content: "should not be added"},
	}, "")
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
		}, "")
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
		followUpKeys:    concurrent.NewMap[string, *idempotencyCache](),
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
		ch, err := sm.RunSession(ctx, sess1.ID, "agent", "root", []api.Message{{Content: "a"}}, "")
		assert.NoError(t, err)
		for range ch {
		}
	}()

	go func() {
		defer wg.Done()
		ch, err := sm.RunSession(ctx, sess2.ID, "agent", "root", []api.Message{{Content: "b"}}, "")
		assert.NoError(t, err)
		for range ch {
		}
	}()

	wg.Wait()

	// Both sessions should have streamed (1 each).
	assert.Equal(t, int32(1), fake1.maxConcurrent.Load())
	assert.Equal(t, int32(1), fake2.maxConcurrent.Load())
}

// recordingFollowUpRuntime records calls to FollowUp so tests can assert
// whether the runtime follow-up queue was used.
type recordingFollowUpRuntime struct {
	fakeRuntime

	mu        sync.Mutex
	followUps []string
}

func (r *recordingFollowUpRuntime) FollowUp(msg runtime.QueuedMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.followUps = append(r.followUps, msg.Content)
	return nil
}

func (r *recordingFollowUpRuntime) followUpContents() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.followUps...)
}

// TestFollowUpSession_RoutesToInjectorWhenRegistered verifies that an
// attached session's follow-up is delivered through the registered injector
// (which starts a real turn in the TUI App) rather than the runtime
// follow-up queue, which an idle session never drains.
func TestFollowUpSession_RoutesToInjectorWhenRegistered(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sess := session.New()
	fake := &recordingFollowUpRuntime{}
	sm := newTestSessionManager(t, sess, &fake.fakeRuntime)
	// Replace the pre-registered runtime with our recording one.
	sm.runtimeSessions.Store(sess.ID, &activeRuntimes{runtime: fake, session: sess})

	var injected []string
	sm.RegisterFollowUpInjector(sess.ID, func(_ context.Context, content string) {
		injected = append(injected, content)
	})

	streaming, duplicate, err := sm.FollowUpSession(ctx, sess.ID, []api.Message{{Content: "do this"}, {Content: "then that"}}, "")
	require.NoError(t, err)

	assert.True(t, streaming, "an injected follow-up always starts/continues a turn")
	assert.False(t, duplicate)
	assert.Equal(t, []string{"do this", "then that"}, injected)
	assert.Empty(t, fake.followUpContents(), "the runtime queue must be bypassed when an injector is registered")
}

// TestFollowUpSession_UsesRuntimeQueueWithoutInjector verifies the headless
// path (no injector): messages go to the runtime follow-up queue.
func TestFollowUpSession_UsesRuntimeQueueWithoutInjector(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sess := session.New()
	fake := &recordingFollowUpRuntime{}
	sm := newTestSessionManager(t, sess, &fake.fakeRuntime)
	sm.runtimeSessions.Store(sess.ID, &activeRuntimes{runtime: fake, session: sess})

	_, _, err := sm.FollowUpSession(ctx, sess.ID, []api.Message{{Content: "queued"}}, "")
	require.NoError(t, err)

	assert.Equal(t, []string{"queued"}, fake.followUpContents())
}

// TestFollowUpSession_UnknownSession returns ErrSessionNotRunning.
func TestFollowUpSession_UnknownSession(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sm := newTestSessionManager(t, sess, &fakeRuntime{})

	_, _, err := sm.FollowUpSession(t.Context(), "does-not-exist", []api.Message{{Content: "x"}}, "")
	assert.ErrorIs(t, err, ErrSessionNotRunning)
}

// TestFollowUpSession_IdempotencyKeyDedupes verifies that two follow-ups with
// the same Idempotency-Key are delivered only once; the second is reported as
// a duplicate.
func TestFollowUpSession_IdempotencyKeyDedupes(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sess := session.New()
	fake := &recordingFollowUpRuntime{}
	sm := newTestSessionManager(t, sess, &fake.fakeRuntime)
	sm.runtimeSessions.Store(sess.ID, &activeRuntimes{runtime: fake, session: sess})

	_, dup1, err := sm.FollowUpSession(ctx, sess.ID, []api.Message{{Content: "once"}}, "key-1")
	require.NoError(t, err)
	assert.False(t, dup1)

	_, dup2, err := sm.FollowUpSession(ctx, sess.ID, []api.Message{{Content: "once"}}, "key-1")
	require.NoError(t, err)
	assert.True(t, dup2, "a repeat with the same key must be a duplicate")

	// A different key is delivered normally.
	_, dup3, err := sm.FollowUpSession(ctx, sess.ID, []api.Message{{Content: "again"}}, "key-2")
	require.NoError(t, err)
	assert.False(t, dup3)

	assert.Equal(t, []string{"once", "again"}, fake.followUpContents(),
		"the deduplicated follow-up must be delivered exactly once")
}

// TestForkSession_CopiesHistoryBeforeUserMessage exercises the happy path:
// forking at the second user message keeps the first user/assistant pair
// and drops everything from the fork point onwards.
func TestForkSession_CopiesHistoryBeforeUserMessage(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	parent := session.New()
	parent.Title = "Parent Title"
	parent.Messages = []session.Item{
		session.NewMessageItem(session.UserMessage("first user")),
		session.NewMessageItem(session.NewAgentMessage("root", &chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "first answer",
		})),
		session.NewMessageItem(session.UserMessage("second user")),
		session.NewMessageItem(session.NewAgentMessage("root", &chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "second answer",
		})),
	}
	require.NoError(t, store.AddSession(ctx, parent))

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})

	// Fork BEFORE the second user message (flat index 2).
	forked, err := sm.ForkSession(ctx, parent.ID, 2)
	require.NoError(t, err)

	assert.NotEqual(t, parent.ID, forked.ID, "fork must have a fresh session ID")
	assert.Equal(t, "Parent Title (fork 1)", forked.Title)

	msgs := forked.GetAllMessages()
	require.Len(t, msgs, 2, "fork must contain only the user/assistant pair before the cut point")
	assert.Equal(t, "first user", msgs[0].Message.Content)
	assert.Equal(t, "first answer", msgs[1].Message.Content)

	// The forked session must be persisted and retrievable.
	loaded, err := store.GetSession(ctx, forked.ID)
	require.NoError(t, err)
	assert.Equal(t, forked.ID, loaded.ID)
}

// TestForkSession_RejectsNonUserMessage pins the contract that fork is
// only valid at a user-role message. Attempting to fork at an assistant
// turn must fail with ErrForkInvalidMessage, regardless of where the
// index points in the flat list.
func TestForkSession_RejectsNonUserMessage(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	parent := session.New()
	parent.Messages = []session.Item{
		session.NewMessageItem(session.UserMessage("hello")),
		session.NewMessageItem(session.NewAgentMessage("root", &chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "hi",
		})),
	}
	require.NoError(t, store.AddSession(ctx, parent))

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})

	// Index 1 points at the assistant message in the flat list — must be
	// rejected even though the position is otherwise valid.
	_, err := sm.ForkSession(ctx, parent.ID, 1)
	require.ErrorIs(t, err, ErrForkInvalidMessage)
}

// TestForkSession_OutOfRange covers the validation boundary: a negative
// index, an index past the end of the visible message list, and the
// "after the last message" position must all fail with ErrForkOutOfRange
// without touching the store. The end-of-list case is the regression
// guard for the dropped full-clone shortcut.
func TestForkSession_OutOfRange(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	parent := session.New()
	parent.Messages = []session.Item{session.NewMessageItem(session.UserMessage("hello"))}
	require.NoError(t, store.AddSession(ctx, parent))

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})

	_, err := sm.ForkSession(ctx, parent.ID, -1)
	require.ErrorIs(t, err, ErrForkOutOfRange)

	_, err = sm.ForkSession(ctx, parent.ID, 5)
	require.ErrorIs(t, err, ErrForkOutOfRange)

	// Equal to the visible-message count: previously a silent full clone,
	// now an explicit ErrForkOutOfRange so the role check can't be bypassed.
	_, err = sm.ForkSession(ctx, parent.ID, 1)
	require.ErrorIs(t, err, ErrForkOutOfRange)
}

// TestForkSession_DeepCopyIsolatesParent verifies that mutating the
// forked session's messages does not leak back into the parent: this is
// the property that makes a fork safe to edit independently.
func TestForkSession_DeepCopyIsolatesParent(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	parent := session.New()
	parent.Messages = []session.Item{
		session.NewMessageItem(session.UserMessage("original")),
		session.NewMessageItem(session.UserMessage("next")),
	}
	require.NoError(t, store.AddSession(ctx, parent))

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})

	forked, err := sm.ForkSession(ctx, parent.ID, 1)
	require.NoError(t, err)
	require.Len(t, forked.Messages, 1)

	forked.Messages[0].Message.Message.Content = "mutated"

	parentReloaded, err := store.GetSession(ctx, parent.ID)
	require.NoError(t, err)
	assert.Equal(t, "original", parentReloaded.Messages[0].Message.Message.Content,
		"mutating the fork must not affect the parent")
}

// TestFlatMessageIndexToItemIndex covers the index translation helper in
// isolation, including the system-message skip rule that mirrors
// Session.GetAllMessages.
func TestFlatMessageIndexToItemIndex(t *testing.T) {
	t.Parallel()

	sess := session.New()
	// Items: [user(0), system(skipped), user(1)]. From a client's
	// perspective the flat indexing is 0 → user "u1", 1 → user "u2".
	sess.Messages = []session.Item{
		session.NewMessageItem(session.UserMessage("u1")),
		session.NewMessageItem(&session.Message{
			Message: chat.Message{Role: chat.MessageRoleSystem, Content: "sys"},
		}),
		session.NewMessageItem(session.UserMessage("u2")),
	}

	idx, err := flatMessageIndexToItemIndex(sess, 0)
	require.NoError(t, err)
	assert.Equal(t, 0, idx)

	idx, err = flatMessageIndexToItemIndex(sess, 1)
	require.NoError(t, err)
	assert.Equal(t, 2, idx, "flat index 1 must skip past the system item")

	// Past the end is rejected, including the "after the last message"
	// position that earlier revisions accepted as a full-clone shortcut.
	// Forks must anchor on a real user turn so the role validation in
	// ForkSession is never bypassed.
	_, err = flatMessageIndexToItemIndex(sess, 2)
	require.ErrorIs(t, err, ErrForkOutOfRange)

	_, err = flatMessageIndexToItemIndex(sess, -1)
	require.ErrorIs(t, err, ErrForkOutOfRange)

	_, err = flatMessageIndexToItemIndex(sess, 3)
	require.ErrorIs(t, err, ErrForkOutOfRange)
}
