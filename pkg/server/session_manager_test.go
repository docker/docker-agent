package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/concurrent"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/sessiontitle"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// fakeRuntime is a minimal Runtime that records concurrent RunStream calls.
type fakeRuntime struct {
	runtime.Runtime

	concurrentStreams atomic.Int32
	maxConcurrent     atomic.Int32
	// release, when non-nil, keeps the stream open until it is closed or
	// the stream context is cancelled; when nil the stream ends at once.
	release chan struct{}
}

func (f *fakeRuntime) RunStream(ctx context.Context, _ *session.Session) <-chan runtime.Event {
	cur := f.concurrentStreams.Add(1)
	for {
		old := f.maxConcurrent.Load()
		if cur <= old || f.maxConcurrent.CompareAndSwap(old, cur) {
			break
		}
	}

	ch := make(chan runtime.Event)
	go func() {
		if f.release != nil {
			select {
			case <-f.release:
			case <-ctx.Done():
			}
		}
		f.concurrentStreams.Add(-1)
		close(ch)
	}()
	return ch
}

func (f *fakeRuntime) Resume(_ context.Context, _ runtime.ResumeRequest) {}

func (f *fakeRuntime) Steer(_ context.Context, _ runtime.QueuedMessage) error { return nil }

func (f *fakeRuntime) FollowUp(_ context.Context, _ runtime.QueuedMessage) error { return nil }

func (f *fakeRuntime) ResumeElicitation(_ context.Context, _ tools.ElicitationAction, _ map[string]any, _ ...string) error {
	return nil
}

// OnElicitationRequest is a no-op: DeleteSession/BatchDeleteSessions call it
// (with nil) on server-owned runtimes to silence the manager's sink.
func (f *fakeRuntime) OnElicitationRequest(func(runtime.Event)) {}

func (f *fakeRuntime) CurrentAgentName(context.Context) string { return "root" }

// SupportsModelSwitching reports false by default. Tests that exercise
// the /models endpoints embed fakeRuntime and override this.
func (f *fakeRuntime) SupportsModelSwitching() bool { return false }

func newTestSessionManager(t *testing.T, sess *session.Session, fake runtime.Runtime) *SessionManager {
	t.Helper()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	require.NoError(t, store.AddSession(ctx, sess))

	sm := &SessionManager{
		runtimeSessions:       concurrent.NewMap[string, *activeRuntimes](),
		deletedSessions:       concurrent.NewMap[string, *activeRuntimes](),
		eventLogs:             concurrent.NewMap[string, *pumpedEventLog](),
		deletedEventLogs:      make(map[string]struct{}),
		followUpInjectors:     concurrent.NewMap[string, FollowUpInjector](),
		followUpKeys:          concurrent.NewMap[string, *idempotencyCache](),
		pendingSafetyDefaults: concurrent.NewMap[string, struct{}](),
		sessionStore:          store,
		Sources:               config.Sources{},
		runConfig:             &config.RuntimeConfig{},
		sessionReady:          make(chan struct{}),
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
	fake := &fakeRuntime{}
	sm.AttachRuntime(t.Context(), sess.ID, fake, sess)

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
	release := make(chan struct{})
	fake := &fakeRuntime{release: release}
	sm := newTestSessionManager(t, sess, fake)

	// Start the first stream. RunSession acquires the streaming lock
	// synchronously, so the session is busy as soon as it returns.
	ch1, err := sm.RunSession(ctx, sess.ID, "agent", "root", []api.Message{
		{Content: "first"},
	}, "")
	require.NoError(t, err)

	// The second request should fail immediately with ErrSessionBusy.
	_, err = sm.RunSession(ctx, sess.ID, "agent", "root", []api.Message{
		{Content: "second"},
	}, "")
	require.ErrorIs(t, err, ErrSessionBusy)

	// Let the first stream complete and drain it.
	close(release)
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
	release := make(chan struct{})
	fake := &fakeRuntime{release: release}
	sm := newTestSessionManager(t, sess, fake)

	ch1, err := sm.RunSession(ctx, sess.ID, "agent", "root", []api.Message{
		{Content: "first"},
	}, "")
	require.NoError(t, err)

	msgCountBefore := len(sess.GetAllMessages())

	_, err = sm.RunSession(ctx, sess.ID, "agent", "root", []api.Message{
		{Content: "should not be added"},
	}, "")
	require.ErrorIs(t, err, ErrSessionBusy)

	// Messages should not have been added.
	assert.Len(t, sess.GetAllMessages(), msgCountBefore)

	close(release)
	for range ch1 {
	}
}

// TestAddMessage_RejectsWhileSessionStreaming verifies the 409-busy guard
// added for issue #3590: AddMessage must reject with ErrSessionBusy while
// the session has an active RunStream. session.Session.mu already makes the
// append itself race-free, but a message injected mid-stream (mid-tool-call
// in particular) can still desynchronize the turn from what the model/tools
// expect, so the API layer also rejects it outright.
func TestAddMessage_RejectsWhileSessionStreaming(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sess := session.New()
	release := make(chan struct{})
	fake := &fakeRuntime{release: release}
	sm := newTestSessionManager(t, sess, fake)

	ch, err := sm.RunSession(ctx, sess.ID, "agent", "root", []api.Message{{Content: "hi"}}, "")
	require.NoError(t, err)

	err = sm.AddMessage(ctx, sess.ID, session.UserMessage("should be rejected"))
	require.ErrorIs(t, err, ErrSessionBusy)

	close(release)
	for range ch {
	}

	// After the stream ends, AddMessage must succeed normally.
	require.NoError(t, sm.AddMessage(ctx, sess.ID, session.UserMessage("accepted")))
}

// TestUpdateMessage_RejectsWhileSessionStreaming mirrors
// TestAddMessage_RejectsWhileSessionStreaming for UpdateMessage.
func TestUpdateMessage_RejectsWhileSessionStreaming(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sess := session.New()
	release := make(chan struct{})
	fake := &fakeRuntime{release: release}
	sm := newTestSessionManager(t, sess, fake)

	msgID, err := sm.sessionStore.AddMessage(ctx, sess.ID, session.UserMessage("original"))
	require.NoError(t, err)
	msgIDStr := strconv.FormatInt(msgID, 10)

	ch, err := sm.RunSession(ctx, sess.ID, "agent", "root", []api.Message{{Content: "hi"}}, "")
	require.NoError(t, err)

	err = sm.UpdateMessage(ctx, sess.ID, msgIDStr, session.UserMessage("should be rejected"))
	require.ErrorIs(t, err, ErrSessionBusy)

	close(release)
	for range ch {
	}

	// After the stream ends, UpdateMessage must succeed normally.
	require.NoError(t, sm.UpdateMessage(ctx, sess.ID, msgIDStr, session.UserMessage("accepted")))
}

// TestAttachedStream_AddMessageAndUpdateMessageRejectWhileStreaming pins the
// fix for the other #3590 blocker: runtimes attached via AttachRuntime
// stream directly through RunStream (see pkg/app.App.Run/Retry/
// RunWithMessage), never going through RunSession, which is the only place
// that used to acquire activeRuntimes.streaming. Before the fix nothing held
// that lock for an attached stream, so AddMessage/UpdateMessage wrongly
// succeeded during a genuinely active attached stream instead of returning
// ErrSessionBusy. AttachRuntime now returns the same lock RunSession uses;
// the App holds it for the duration of every direct RunStream call (see
// app.WithStreamGuard/acquireStreamGuard). This test drives a real stream
// through that lock exactly the way the App does — NOT through
// sm.RunSession — to prove the guard covers the attached path too.
func TestAttachedStream_AddMessageAndUpdateMessageRejectWhileStreaming(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))

	msgID, err := store.AddMessage(ctx, sess.ID, session.UserMessage("original"))
	require.NoError(t, err)
	msgIDStr := strconv.FormatInt(msgID, 10)

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})
	release := make(chan struct{})
	fake := &fakeRuntime{release: release}
	guard := sm.AttachRuntime(ctx, sess.ID, fake, sess)

	// Simulate the TUI/attached owner streaming directly through the
	// runtime, exactly like pkg/app.App.acquireStreamGuard + Run do — NOT
	// through sm.RunSession.
	guard.Lock()
	ch := fake.RunStream(ctx, sess)

	err = sm.AddMessage(ctx, sess.ID, session.UserMessage("should be rejected"))
	require.ErrorIs(t, err, ErrSessionBusy)

	err = sm.UpdateMessage(ctx, sess.ID, msgIDStr, session.UserMessage("should be rejected"))
	require.ErrorIs(t, err, ErrSessionBusy)

	close(release)
	for range ch {
	}
	guard.Unlock()

	// After the attached stream ends, both must succeed normally.
	require.NoError(t, sm.AddMessage(ctx, sess.ID, session.UserMessage("accepted")))
	require.NoError(t, sm.UpdateMessage(ctx, sess.ID, msgIDStr, session.UserMessage("accepted")))
}

// blockingStore wraps a session.Store and blocks inside AddMessage/
// UpdateMessage until release is closed, letting a test pause the manager
// mid-mutation — after the busy check has already passed — to observe
// whether a concurrent attached stream can slip in before the mutation
// actually completes. entered is closed the instant the blocked call is
// reached, so the test can synchronize on it instead of sleeping.
type blockingStore struct {
	session.Store

	release chan struct{}
	entered chan struct{}
}

func (s *blockingStore) AddMessage(ctx context.Context, sessionID string, msg *session.Message) (int64, error) {
	close(s.entered)
	<-s.release
	return s.Store.AddMessage(ctx, sessionID, msg)
}

func (s *blockingStore) UpdateMessage(ctx context.Context, messageID int64, msg *session.Message) error {
	close(s.entered)
	<-s.release
	return s.Store.UpdateMessage(ctx, messageID, msg)
}

// assertAttachedGuardBlockedDuringMutation drives the reviewer's
// deterministic "blocking-store" probe (#3590 finding A1): it starts
// mutate (an AddMessage or UpdateMessage call) against a store that blocks
// mid-write, waits for the busy check inside mutate to have already passed
// (store.entered closes), and then tries to acquire the attached-stream
// guard exactly the way pkg/app.App.acquireStreamGuard does (a plain
// Lock(), not TryLock()). Before the #3590 fix, AddMessage/UpdateMessage
// released activeRuntimes.streaming immediately after the busy check, so
// the guard acquisition below would succeed while mutate was still
// blocked inside the store write — an attached stream starting between
// the busy check and the mutation completing. With the fix (the streaming
// lock held via defer until mutate returns), the guard acquisition must
// stay blocked until mutate has fully returned.
func assertAttachedGuardBlockedDuringMutation(t *testing.T, guard sync.Locker, store *blockingStore, mutate func() error) {
	t.Helper()

	mutateErrCh := make(chan error, 1)
	go func() { mutateErrCh <- mutate() }()

	select {
	case <-store.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the mutation to reach the blocking store")
	}

	guardAcquired := make(chan struct{})
	go func() {
		guard.Lock()
		close(guardAcquired)
	}()

	select {
	case <-guardAcquired:
		t.Fatal("attached stream guard acquired before the REST mutation completed")
	case <-time.After(100 * time.Millisecond):
		// Expected: the guard stays held across the in-flight mutation.
	}

	close(store.release)
	require.NoError(t, <-mutateErrCh)

	select {
	case <-guardAcquired:
		guard.Unlock()
	case <-time.After(2 * time.Second):
		t.Fatal("attached stream guard never acquired after the REST mutation completed")
	}
}

// TestReview_AttachedGuardCannotStartBetweenBusyCheckAndMutation_AddMessage
// is the reviewer's deterministic regression probe for #3590 finding A1:
// AddMessage must hold activeRuntimes.streaming across its entire mutation,
// not just across the busy check, otherwise an attached stream (the only
// consumer of that lock outside RunSession — see AttachRuntime) can start
// in the gap between the check passing and the store write completing.
func TestReview_AttachedGuardCannotStartBetweenBusyCheckAndMutation_AddMessage(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sess := session.New()
	inner := session.NewInMemorySessionStore()
	require.NoError(t, inner.AddSession(ctx, sess))
	store := &blockingStore{Store: inner, release: make(chan struct{}), entered: make(chan struct{})}

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})
	guard := sm.AttachRuntime(ctx, sess.ID, &fakeRuntime{}, sess)

	assertAttachedGuardBlockedDuringMutation(t, guard, store, func() error {
		return sm.AddMessage(ctx, sess.ID, session.UserMessage("mutating"))
	})
}

// TestReview_AttachedGuardCannotStartBetweenBusyCheckAndMutation_UpdateMessage
// mirrors the AddMessage probe above for UpdateMessage.
func TestReview_AttachedGuardCannotStartBetweenBusyCheckAndMutation_UpdateMessage(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sess := session.New()
	inner := session.NewInMemorySessionStore()
	require.NoError(t, inner.AddSession(ctx, sess))
	msgID, err := inner.AddMessage(ctx, sess.ID, session.UserMessage("original"))
	require.NoError(t, err)
	msgIDStr := strconv.FormatInt(msgID, 10)

	store := &blockingStore{Store: inner, release: make(chan struct{}), entered: make(chan struct{})}

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})
	guard := sm.AttachRuntime(ctx, sess.ID, &fakeRuntime{}, sess)

	assertAttachedGuardBlockedDuringMutation(t, guard, store, func() error {
		return sm.UpdateMessage(ctx, sess.ID, msgIDStr, session.UserMessage("mutating"))
	})
}

// TestServer_AddMessage_Returns409WhileSessionStreaming and
// TestServer_UpdateMessage_Returns409WhileSessionStreaming drive the actual
// HTTP handlers (not just SessionManager) to pin the 409-busy guard added
// for issue #3590 end to end: ErrSessionBusy from the manager must surface
// as echo.NewHTTPError(http.StatusConflict, ...), mirroring how runAgent
// already maps it.
func TestServer_AddMessage_Returns409WhileSessionStreaming(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sess := session.New()
	release := make(chan struct{})
	fake := &fakeRuntime{release: release}
	sm := newTestSessionManager(t, sess, fake)
	srv := NewWithManager(sm, "")

	ch, err := sm.RunSession(ctx, sess.ID, "agent", "root", []api.Message{{Content: "hi"}}, "")
	require.NoError(t, err)

	body, err := json.Marshal(api.AddMessageRequest{Message: session.UserMessage("should be rejected")})
	require.NoError(t, err)

	e := echo.New()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/sessions/"+sess.ID+"/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues(sess.ID)

	err = srv.addMessage(c)
	var httpErr *echo.HTTPError
	require.ErrorAs(t, err, &httpErr)
	assert.Equal(t, http.StatusConflict, httpErr.Code)

	close(release)
	for range ch {
	}
}

func TestServer_UpdateMessage_Returns409WhileSessionStreaming(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sess := session.New()
	release := make(chan struct{})
	fake := &fakeRuntime{release: release}
	sm := newTestSessionManager(t, sess, fake)
	srv := NewWithManager(sm, "")

	msgID, err := sm.sessionStore.AddMessage(ctx, sess.ID, session.UserMessage("original"))
	require.NoError(t, err)

	ch, err := sm.RunSession(ctx, sess.ID, "agent", "root", []api.Message{{Content: "hi"}}, "")
	require.NoError(t, err)

	body, err := json.Marshal(api.UpdateMessageRequest{Message: session.UserMessage("should be rejected")})
	require.NoError(t, err)

	e := echo.New()
	msgIDStr := strconv.FormatInt(msgID, 10)
	req := httptest.NewRequestWithContext(ctx, http.MethodPatch, "/api/sessions/"+sess.ID+"/messages/"+msgIDStr, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id", "msg_id")
	c.SetParamValues(sess.ID, msgIDStr)

	err = srv.updateMessage(c)
	var httpErr *echo.HTTPError
	require.ErrorAs(t, err, &httpErr)
	assert.Equal(t, http.StatusConflict, httpErr.Code)

	close(release)
	for range ch {
	}
}

// TestRunSession_SequentialRequestsSucceed verifies that sequential
// (non-overlapping) requests on the same session work normally.
func TestRunSession_SequentialRequestsSucceed(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sess := session.New()
	fake := &fakeRuntime{}
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
	fake1 := &fakeRuntime{}
	fake2 := &fakeRuntime{}

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

func (r *recordingFollowUpRuntime) FollowUp(_ context.Context, msg runtime.QueuedMessage) error {
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

func TestRecallSession_DeleteCancelsRecalledRunAndDoesNotResurrect(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sess := session.New()
	fake := &fakeRuntime{release: make(chan struct{})}
	sm := newTestSessionManager(t, sess, fake)

	recall := runtime.QueuedMessage{Content: "background job finished"}
	require.NoError(t, sm.recallSession(ctx, sess.ID, recall))
	require.Eventually(t, func() bool {
		return fake.concurrentStreams.Load() == 1
	}, time.Second, 10*time.Millisecond)

	require.NoError(t, sm.DeleteSession(ctx, sess.ID))
	require.NoError(t, sm.WaitStopped(ctx, sess.ID, time.Second))

	_, err := sm.sessionStore.GetSession(ctx, sess.ID)
	assert.ErrorIs(t, err, session.ErrNotFound)
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

	// Fork BEFORE the second user message (user-message ordinal 1).
	forked, err := sm.ForkSession(ctx, parent.ID, 1)
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

// TestForkSession_ConcurrentWithLiveSessionMutation pins the data-race fix
// for issue #3590: InMemorySessionStore.GetSession returns the live, shared
// *Session pointer (not a copy), so ForkSession's index computation
// (userMessageOrdinalToItemIndex) and session.ForkSession's own copy must
// both go through locked snapshots to stay safe against a concurrent
// AddMessage on that same live session — e.g. the HTTP AddMessage handler
// racing a TUI fork action. Run with -race; before the fix, iterating
// s.Messages directly races the concurrent AddMessage goroutine below.
func TestForkSession_ConcurrentWithLiveSessionMutation(t *testing.T) {
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
	}
	require.NoError(t, store.AddSession(ctx, parent))

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			parent.AddMessage(session.UserMessage("concurrent"))
		})
		wg.Go(func() {
			if _, err := sm.ForkSession(ctx, parent.ID, 0); err != nil {
				t.Errorf("ForkSession: %v", err)
			}
		})
	}
	wg.Wait()
}

// Regression: repeated forks of the same parent must pick (fork 1),
// (fork 2), (fork 3) rather than three copies of (fork 1).
func TestForkSession_TitleIncrementsAcrossSiblings(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	parent := session.New()
	parent.Title = "Original"
	parent.Messages = []session.Item{
		session.NewMessageItem(session.UserMessage("u1")),
		session.NewMessageItem(session.NewAgentMessage("root", &chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "a1",
		})),
		session.NewMessageItem(session.UserMessage("u2")),
		session.NewMessageItem(session.NewAgentMessage("root", &chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "a2",
		})),
		session.NewMessageItem(session.UserMessage("u3")),
	}
	require.NoError(t, store.AddSession(ctx, parent))

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})

	fork1, err := sm.ForkSession(ctx, parent.ID, 0)
	require.NoError(t, err)
	assert.Equal(t, "Original (fork 1)", fork1.Title)

	fork2, err := sm.ForkSession(ctx, parent.ID, 1)
	require.NoError(t, err)
	assert.Equal(t, "Original (fork 2)", fork2.Title)

	fork3, err := sm.ForkSession(ctx, parent.ID, 2)
	require.NoError(t, err)
	assert.Equal(t, "Original (fork 3)", fork3.Title)

	// Forking a fork shares the counter rather than restarting at 1.
	forkOfFork, err := sm.ForkSession(ctx, fork2.ID, 0)
	require.NoError(t, err)
	assert.Equal(t, "Original (fork 4)", forkOfFork.Title)
}

// TestForkSession_OutOfRange covers the validation boundary: negative,
// past-the-end, and equal-to-count ordinals must all fail with
// ErrForkOutOfRange. The equal-to-count case is the regression guard
// for the dropped full-clone shortcut.
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

	// Equal to the visible user-message count: previously a silent full
	// clone, now an explicit ErrForkOutOfRange so the contract stays
	// "anchor on a real user turn".
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

// TestUserMessageOrdinalToItemIndex covers the ordinal translation
// helper: only user-role messages count; system and assistant items
// are skipped; out-of-range ordinals are rejected.
func TestUserMessageOrdinalToItemIndex(t *testing.T) {
	t.Parallel()

	sess := session.New()
	// Items 0..3: user, system, assistant, user. Ordinal 0 → item 0,
	// ordinal 1 → item 3.
	sess.Messages = []session.Item{
		session.NewMessageItem(session.UserMessage("u1")),
		session.NewMessageItem(&session.Message{
			Message: chat.Message{Role: chat.MessageRoleSystem, Content: "sys"},
		}),
		session.NewMessageItem(session.NewAgentMessage("root", &chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "a1",
		})),
		session.NewMessageItem(session.UserMessage("u2")),
	}

	idx, err := userMessageOrdinalToItemIndex(sess, 0)
	require.NoError(t, err)
	assert.Equal(t, 0, idx)

	idx, err = userMessageOrdinalToItemIndex(sess, 1)
	require.NoError(t, err)
	assert.Equal(t, 3, idx, "ordinal 1 must skip past the system and assistant items")

	_, err = userMessageOrdinalToItemIndex(sess, 2)
	require.ErrorIs(t, err, ErrForkOutOfRange)

	_, err = userMessageOrdinalToItemIndex(sess, -1)
	require.ErrorIs(t, err, ErrForkOutOfRange)

	_, err = userMessageOrdinalToItemIndex(sess, 99)
	require.ErrorIs(t, err, ErrForkOutOfRange)
}

// TestAddMessage_SQLitePersistedToolResultCappedOnReload pins the read-time
// backstop for the generic API path: SessionManager.AddMessage persists a
// tool result through the store without Session.AddMessage's ingest-time
// cap, so a session reloaded from SQLite carries the unbounded payload.
// Once the runtime stamps the agent's MaxToolResultTokens onto the session
// (runtimeForSession), GetMessages — what actually reaches a provider —
// must bound the result while the persisted history stays as stored.
func TestAddMessage_SQLitePersistedToolResultCappedOnReload(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, err := sqlitestore.New(ctx, filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	sess := session.New(session.WithUserMessage("run big-tool"))
	sess.AddMessage(session.NewAgentMessage("root", &chat.Message{
		Role: chat.MessageRoleAssistant,
		ToolCalls: []tools.ToolCall{
			{ID: "tc", Function: tools.FunctionCall{Name: "big_tool", Arguments: "{}"}},
		},
	}))
	require.NoError(t, store.AddSession(ctx, sess))

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})
	oversized := "HEAD" + strings.Repeat("x", 100_000) + "TAIL"
	require.NoError(t, sm.AddMessage(ctx, sess.ID, session.NewAgentMessage("root", &chat.Message{
		Role:       chat.MessageRoleTool,
		ToolCallID: "tc",
		Content:    oversized,
	})))

	reloaded, err := store.GetSession(ctx, sess.ID)
	require.NoError(t, err)
	assert.Equal(t, oversized, storedToolResultContent(t, reloaded, "tc"),
		"persistence must keep the raw result; only what reaches the provider is capped")

	// runtimeForSession copies the agent's cap onto the session before a run.
	agt := agent.New("root", "test instruction", agent.WithMaxToolResultTokens(100))
	reloaded.MaxToolResultTokens = agt.MaxToolResultTokens()

	var got string
	for _, msg := range reloaded.GetMessages(agt) {
		if msg.Role == chat.MessageRoleTool && msg.ToolCallID == "tc" {
			got = msg.Content
		}
	}
	require.NotEmpty(t, got, "tool result must survive sanitizeToolCalls")
	assert.LessOrEqual(t, len(got)/4, 100, "reloaded result must be bounded by the cap")
	assert.True(t, strings.HasPrefix(got, "HEAD"), "head must survive middle-out truncation")
	assert.True(t, strings.HasSuffix(got, "TAIL"), "tail must survive middle-out truncation")

	assert.Equal(t, oversized, storedToolResultContent(t, reloaded, "tc"),
		"GetMessages must not rewrite the in-memory history either")
}

func storedToolResultContent(t *testing.T, sess *session.Session, toolCallID string) string {
	t.Helper()

	for _, msg := range sess.GetAllMessages() {
		if msg.Message.Role == chat.MessageRoleTool && msg.Message.ToolCallID == toolCallID {
			return msg.Message.Content
		}
	}

	require.Failf(t, "tool result not found", "tool_call_id=%s", toolCallID)
	return ""
}

// --- #3584: session-scoped elicitation sink for API/server-created runtimes ---

// scriptedStreamRuntime is a fakeRuntime whose RunStream replays a fixed
// list of events and closes.
type scriptedStreamRuntime struct {
	fakeRuntime

	events []runtime.Event
}

func (s *scriptedStreamRuntime) RunStream(context.Context, *session.Session) <-chan runtime.Event {
	ch := make(chan runtime.Event, len(s.events))
	for _, ev := range s.events {
		ch <- ev
	}
	close(ch)
	return ch
}

// mirroredScriptedStreamRuntime additionally declares — like LocalRuntime —
// that its OnElicitationRequest sink is the exactly-once delivery point and
// any elicitation copy on RunStream is a best-effort mirror.
type mirroredScriptedStreamRuntime struct {
	scriptedStreamRuntime
}

func (m *mirroredScriptedStreamRuntime) MirrorsElicitationOnRunStream() {}

// waitSessionIdle blocks until sessionID's streaming lock is free, i.e. the
// recall/run pump goroutine has fully finished appending to the event log.
func waitSessionIdle(t *testing.T, sm *SessionManager, sessionID string) {
	t.Helper()
	require.Eventually(t, func() bool {
		rt, ok := sm.runtimeSessions.Load(sessionID)
		if !ok {
			return false
		}
		if !rt.streaming.TryLock() {
			return false
		}
		rt.streaming.Unlock()
		return true
	}, 2*time.Second, time.Millisecond)
}

// replaySessionEvents streams sessionID's event log until want events have
// been received (replay first, then live tail), then returns them in order.
func replaySessionEvents(t *testing.T, sm *SessionManager, sessionID string, want int) []any {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var mu sync.Mutex
	var events []any
	done := make(chan struct{})
	go func() {
		defer close(done)
		ok := sm.StreamEvents(ctx, sessionID, nil, func(_ uint64, event any) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, event)
		})
		assert.True(t, ok, "expected an event log for session %s", sessionID)
	}()
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(events) >= want
	}, 2*time.Second, time.Millisecond)
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	return append([]any(nil), events...)
}

// TestSessionElicitationSink_MakesSessionEventSourceReplayable pins the fix
// for #3584 on the server side: before it, only pkg/app (the TUI) ever
// registered an OnElicitationRequest sink, so every API/server-created
// runtime had none — elicitationHandler's headless fast-decline path ("no
// sink means no UI") therefore fired for every background elicitation raised
// through the API, even though an HTTP client could otherwise answer it.
// runtimeForSession registers sessionElicitationSink on every runtime it
// builds; this exercises that exact closure (without needing a full
// runtime/team) and confirms the session gains a replayable event source it
// didn't have before.
func TestSessionElicitationSink_MakesSessionEventSourceReplayable(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sm := NewSessionManager(ctx, config.Sources{}, session.NewInMemorySessionStore(), 0, &config.RuntimeConfig{})

	require.False(t, sm.HasEventSource("sess-1"),
		"a session that was never attached and produced no out-of-band event must have no event source")

	sink := sm.sessionElicitationSink("sess-1")
	ev := runtime.ElicitationRequest("need input", "form", nil, "", "eid-1", "", "bg-child", nil, "agent")
	sink(ev)

	require.True(t, sm.HasEventSource("sess-1"),
		"the sink must lazily create a session-scoped event source on first use")

	replayed := replaySessionEvents(t, sm, "sess-1", 1)
	require.Len(t, replayed, 1)
	assert.Same(t, ev, replayed[0], "the elicitation event must be replayable via GET .../events")
}

// TestSessionElicitationSink_AccumulatesAcrossCalls verifies repeated sink
// invocations append to one session-scoped log — each event exactly once, in
// order — rather than each invocation overwriting the log.
func TestSessionElicitationSink_AccumulatesAcrossCalls(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sm := NewSessionManager(ctx, config.Sources{}, session.NewInMemorySessionStore(), 0, &config.RuntimeConfig{})

	sink := sm.sessionElicitationSink("sess-2")
	first := runtime.ElicitationRequest("first", "form", nil, "", "eid-1", "", "bg-child-1", nil, "agent")
	second := runtime.ElicitationRequest("second", "form", nil, "", "eid-2", "", "bg-child-2", nil, "agent")
	sink(first)
	sink(second)

	seq, ok := sm.LastEventSeq("sess-2")
	require.True(t, ok)
	assert.Equal(t, uint64(2), seq, "both sink deliveries must land in the same event log")

	replayed := replaySessionEvents(t, sm, "sess-2", 2)
	require.Len(t, replayed, 2)
	assert.Same(t, first, replayed[0])
	assert.Same(t, second, replayed[1])
}

// TestSessionElicitationSink_ReusesAttachedEventLog verifies the sink never
// clobbers a log that RegisterEventSource already attached (the --listen
// case): the elicitation must continue that log's sequence instead of
// replacing it — replacing would silently drop the attached pump's buffered
// events and detach its cancel.
func TestSessionElicitationSink_ReusesAttachedEventLog(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sm := NewSessionManager(ctx, config.Sources{}, session.NewInMemorySessionStore(), 0, &config.RuntimeConfig{})

	pumped := make(chan struct{})
	sm.RegisterEventSource("sess-3", func(ctx context.Context, send func(any)) {
		send("attached-event")
		close(pumped)
		<-ctx.Done() // keep the source alive so the pump doesn't close the log
	})
	// The pump ctx is detached from t.Context(); stop it explicitly.
	t.Cleanup(func() {
		if pe, ok := sm.eventLogs.Load("sess-3"); ok {
			pe.cancel()
		}
	})
	<-pumped

	attached, ok := sm.eventLogs.Load("sess-3")
	require.True(t, ok)

	ev := runtime.ElicitationRequest("need input", "form", nil, "", "eid-1", "", "bg-child", nil, "agent")
	sm.sessionElicitationSink("sess-3")(ev)

	current, ok := sm.eventLogs.Load("sess-3")
	require.True(t, ok)
	assert.Same(t, attached, current, "the sink must reuse the attached event log, not overwrite it")

	seq, ok := sm.LastEventSeq("sess-3")
	require.True(t, ok)
	assert.Equal(t, uint64(2), seq, "the elicitation must continue the attached log's sequence")

	replayed := replaySessionEvents(t, sm, "sess-3", 2)
	require.Len(t, replayed, 2)
	assert.Equal(t, "attached-event", replayed[0])
	assert.Same(t, ev, replayed[1])
}

// TestRuntimeForSession_RegistersSessionScopedElicitationSink is an
// end-to-end check that runtimeForSession actually wires
// sessionElicitationSink onto every runtime it builds (not just that the
// sink mechanics work in isolation, per the tests above). It uses a
// harness-backed agent (harness: type: claude-code) so no model provider or
// API key is needed — runtime construction only requires *a* valid agent,
// per LocalRuntime's "has no valid model" guard.
//
// Crucially, this drives the sink through the RETURNED runtime itself (via
// EmitElicitationRequestForTesting) rather than reconstructing a fresh
// sm.sessionElicitationSink(...) closure and calling that in isolation: the
// latter would keep passing even if runtimeForSession's
// `run.OnElicitationRequest(...)` registration were deleted, since it never
// exercises what got wired onto `run`.
func TestRuntimeForSession_RegistersSessionScopedElicitationSink(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	cfg := []byte(`agents:
  root:
    description: Test agent
    instruction: Be helpful.
    harness:
      type: claude-code
`)
	store := session.NewInMemorySessionStore()
	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))

	sources := config.Sources{"agent.yaml": config.NewBytesSource("agent.yaml", cfg)}
	sm := NewSessionManager(ctx, sources, store, 0, &config.RuntimeConfig{})

	require.False(t, sm.HasEventSource(sess.ID))

	run, _, _, err := sm.runtimeForSession(ctx, sess, "agent.yaml", "", &config.RuntimeConfig{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = run.Close() })

	lr, ok := run.(*runtime.LocalRuntime)
	require.True(t, ok, "runtimeForSession is expected to build a *runtime.LocalRuntime, got %T", run)

	// Simulate what elicitationHandler does when it raises a background
	// elicitation, but through the sink runtimeForSession actually registered
	// on this specific runtime instance, not a freshly built one.
	lr.EmitElicitationRequestForTesting(runtime.ElicitationRequest("need input", "form", nil, "", "eid-1", "", sess.ID, nil, "root"))
	require.True(t, sm.HasEventSource(sess.ID),
		"runtimeForSession must leave the session able to surface out-of-band elicitations")
}

// elicitationRecordingRuntime records ResumeElicitation calls so tests can
// assert exactly what the elicitation endpoint routed to a session's runtime.
type elicitationRecordingRuntime struct {
	fakeRuntime

	mu    sync.Mutex
	calls []recordedElicitationResume
}

type recordedElicitationResume struct {
	action  tools.ElicitationAction
	content map[string]any
	id      string
}

func (e *elicitationRecordingRuntime) ResumeElicitation(_ context.Context, action tools.ElicitationAction, content map[string]any, elicitationID ...string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	var id string
	if len(elicitationID) > 0 {
		id = elicitationID[0]
	}
	e.calls = append(e.calls, recordedElicitationResume{action: action, content: content, id: id})
	return nil
}

func (e *elicitationRecordingRuntime) recordedCalls() []recordedElicitationResume {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]recordedElicitationResume(nil), e.calls...)
}

// TestElicitationEndpoint_RoutesAnswerToSessionRuntime pins the server half
// of the #3584 answer path: POST /api/sessions/:id/elicitation must hand the
// client's action, content and elicitation_id verbatim to the session's
// runtime via ResumeElicitation. Together with pkg/runtime's
// TestElicitationHandler_BackgroundWithSinkStillWaitsForResponse — which
// proves a runtime with a wired OnElicitationRequest sink parks a background
// elicitation until ResumeElicitation(elicitation_id) delivers the answer —
// and TestRuntimeForSession_RegistersSessionScopedElicitationSink above,
// this closes the end-to-end loop without a package-crossing test seam into
// the unexported elicitationHandler.
func TestElicitationEndpoint_RoutesAnswerToSessionRuntime(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sess := session.New()
	fake := &elicitationRecordingRuntime{}
	sm := newTestSessionManager(t, sess, fake)
	srv := NewWithManager(sm, "")

	body, err := json.Marshal(api.ResumeElicitationRequest{
		Action:        "accept",
		Content:       map[string]any{"confirmed": true},
		ElicitationID: "eid-42",
	})
	require.NoError(t, err)

	e := echo.New()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/sessions/"+sess.ID+"/elicitation", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues(sess.ID)
	require.NoError(t, srv.elicitation(c))
	assert.Equal(t, http.StatusOK, rec.Code)

	calls := fake.recordedCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, tools.ElicitationActionAccept, calls[0].action)
	assert.Equal(t, map[string]any{"confirmed": true}, calls[0].content)
	assert.Equal(t, "eid-42", calls[0].id, "the elicitation_id must reach the runtime for per-request routing")
}

// TestRecallSession_SkipsMirroredElicitationOnRunStream guards the
// exactly-once contract of the event log now that the OnElicitationRequest
// sink appends elicitation requests to it: LocalRuntime ALSO best-effort
// mirrors the same event onto RunStream (for out-of-process RunStream
// consumers), so recallSession's pump — which copies RunStream events into
// the same log — must skip that mirror copy for runtimes that declare it
// (MirrorsElicitationOnRunStream), mirroring pkg/app's dedupe.
func TestRecallSession_SkipsMirroredElicitationOnRunStream(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sess := session.New()
	elicit := runtime.ElicitationRequest("need input", "form", nil, "", "eid-1", "", "bg-child", nil, "root")
	other := runtime.Warning("heads up", "root")
	fake := &mirroredScriptedStreamRuntime{scriptedStreamRuntime{events: []runtime.Event{elicit, other}}}
	sm := newTestSessionManager(t, sess, fake)

	// Reliable path: the sink already delivered the request exactly once.
	sm.sessionElicitationSink(sess.ID)(elicit)

	require.NoError(t, sm.recallSession(ctx, sess.ID, runtime.QueuedMessage{Content: "wake up"}))
	waitSessionIdle(t, sm, sess.ID)

	seq, ok := sm.LastEventSeq(sess.ID)
	require.True(t, ok)
	require.Equal(t, uint64(2), seq,
		"the RunStream mirror copy of an elicitation must not be appended a second time")

	replayed := replaySessionEvents(t, sm, sess.ID, 2)
	assert.Same(t, elicit, replayed[0], "the sink-delivered elicitation must be first")
	assert.Same(t, other, replayed[1], "non-elicitation stream events must still be pumped")
}

// TestRecallSession_KeepsElicitationFromNonMirroringRuntime is the
// counterpart guard: a runtime that does NOT declare
// MirrorsElicitationOnRunStream (e.g. RemoteRuntime, whose
// OnElicitationRequest is a no-op) delivers elicitations ONLY via RunStream,
// so that sole copy must reach the event log unfiltered.
func TestRecallSession_KeepsElicitationFromNonMirroringRuntime(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sess := session.New()
	elicit := runtime.ElicitationRequest("need input", "form", nil, "", "eid-1", "", "bg-child", nil, "root")
	fake := &scriptedStreamRuntime{events: []runtime.Event{elicit}}
	sm := newTestSessionManager(t, sess, fake)

	// Pre-create the session's event log (as an attached session would have).
	sm.ensureEventLog(sess.ID)

	require.NoError(t, sm.recallSession(ctx, sess.ID, runtime.QueuedMessage{Content: "wake up"}))
	waitSessionIdle(t, sm, sess.ID)

	seq, ok := sm.LastEventSeq(sess.ID)
	require.True(t, ok)
	require.Equal(t, uint64(1), seq,
		"a non-mirroring runtime's RunStream copy is the only delivery and must not be skipped")

	replayed := replaySessionEvents(t, sm, sess.ID, 1)
	assert.Same(t, elicit, replayed[0])
}

// TestDeleteSession_ClosesLazyElicitationEventLog guards ensureEventLog's
// cancel contract: DeleteSession calls pe.cancel() unconditionally, expecting
// it to end the log's lifetime. Were the lazily-created log's cancel a no-op,
// a client already streaming GET /api/sessions/:id/events for such a session
// would never receive the terminal session_exited event and never see its
// stream close, contradicting the end-of-session contract documented on
// Server.sessionEvents. This proves the lazily-created log is actually
// closed on deletion: session_exited is delivered and the blocked
// StreamEvents call returns.
func TestDeleteSession_ClosesLazyElicitationEventLog(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})

	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))

	// Lazily create the session's event log the way runtimeForSession's
	// sessionElicitationSink does for a background job's elicitation — no
	// runtime/RegisterEventSource pump is ever registered for this session.
	sm.sessionElicitationSink(sess.ID)(runtime.ElicitationRequest("need input", "form", nil, "", "eid-1", "", sess.ID, nil, "root"))
	require.True(t, sm.HasEventSource(sess.ID))

	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	var mu sync.Mutex
	var received []any
	streamDone := make(chan struct{})
	go func() {
		defer close(streamDone)
		sm.StreamEvents(streamCtx, sess.ID, nil, func(_ uint64, event any) {
			mu.Lock()
			defer mu.Unlock()
			received = append(received, event)
		})
	}()

	// Wait for the replay of the elicitation event so the stream is known to
	// be actively connected (not merely about to start) before we delete.
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(received) == 1
	}, 2*time.Second, time.Millisecond)

	require.NoError(t, sm.DeleteSession(ctx, sess.ID))

	select {
	case <-streamDone:
	case <-time.After(2 * time.Second):
		t.Fatal("StreamEvents must return once the session is deleted; a no-op cancel on the lazily-created event log leaves connected /events streams blocked forever")
	}

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, received, 2, "expected the elicitation event followed by a terminal session_exited event")
	exited, ok := received[1].(sessionExitedEvent)
	require.True(t, ok, "expected sessionExitedEvent, got %T", received[1])
	assert.Equal(t, "session_exited", exited.Type)
}

// TestBatchDeleteSessions_ClosesLazyElicitationEventLog is the batch variant
// of the above: BatchDeleteSessions goes through the same pe.cancel() call
// per session, so it must uphold the same contract.
func TestBatchDeleteSessions_ClosesLazyElicitationEventLog(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})

	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))

	sm.sessionElicitationSink(sess.ID)(runtime.ElicitationRequest("need input", "form", nil, "", "eid-1", "", sess.ID, nil, "root"))
	require.True(t, sm.HasEventSource(sess.ID))

	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	var mu sync.Mutex
	var received []any
	streamDone := make(chan struct{})
	go func() {
		defer close(streamDone)
		sm.StreamEvents(streamCtx, sess.ID, nil, func(_ uint64, event any) {
			mu.Lock()
			defer mu.Unlock()
			received = append(received, event)
		})
	}()

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(received) == 1
	}, 2*time.Second, time.Millisecond)

	deleted, failed := sm.BatchDeleteSessions(ctx, []string{sess.ID})
	assert.Equal(t, 1, deleted)
	assert.Empty(t, failed)

	select {
	case <-streamDone:
	case <-time.After(2 * time.Second):
		t.Fatal("StreamEvents must return once the session is batch-deleted; a no-op cancel on the lazily-created event log leaves connected /events streams blocked forever")
	}

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, received, 2, "expected the elicitation event followed by a terminal session_exited event")
	exited, ok := received[1].(sessionExitedEvent)
	require.True(t, ok, "expected sessionExitedEvent, got %T", received[1])
	assert.Equal(t, "session_exited", exited.Type)
}

// --- #3584 review: deletion vs. lazy event-log creation race ---

// TestDeleteSession_StaleSinkAfterDeleteCannotResurrectEventLog pins the
// tombstone gate on ensureEventLog: the elicitation sink closure that
// runtimeForSession registered keeps existing after DeleteSession (a
// detached background elicitation may already hold it), and before the gate
// its next invocation silently recreated a permanent event log for the
// deleted session — an entry nobody would ever tear down again (session IDs
// are never reused) carrying a request nobody could answer (the runtime
// registration is gone). Both the "log existed and was torn down" and the
// "log never existed" orderings must refuse to (re)create one.
func TestDeleteSession_StaleSinkAfterDeleteCannotResurrectEventLog(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})

	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))

	// The exact closure runtimeForSession registers on the runtime; deletion
	// cannot revoke copies already held by in-flight elicitations.
	sink := sm.sessionElicitationSink(sess.ID)
	ev := runtime.ElicitationRequest("need input", "form", nil, "", "eid-1", "", sess.ID, nil, "root")
	sink(ev)
	require.True(t, sm.HasEventSource(sess.ID))

	require.NoError(t, sm.DeleteSession(ctx, sess.ID))
	require.False(t, sm.HasEventSource(sess.ID))

	sink(ev)
	assert.False(t, sm.HasEventSource(sess.ID),
		"a stale sink invocation after DeleteSession must not resurrect the event log")

	// Never-had-a-log ordering: delete first, sink fires afterwards.
	other := session.New()
	require.NoError(t, store.AddSession(ctx, other))
	lateSink := sm.sessionElicitationSink(other.ID)
	require.NoError(t, sm.DeleteSession(ctx, other.ID))

	lateSink(ev)
	assert.False(t, sm.HasEventSource(other.ID),
		"a stale sink invocation must not create an event log for a session deleted before its first event")
}

// TestBatchDeleteSessions_StaleSinkAfterDeleteCannotResurrectEventLog is the
// BatchDeleteSessions counterpart of the test above: batch deletion goes
// through the same tombstone + teardown, so both orderings must hold there
// too.
func TestBatchDeleteSessions_StaleSinkAfterDeleteCannotResurrectEventLog(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})

	withLog := session.New()
	withoutLog := session.New()
	require.NoError(t, store.AddSession(ctx, withLog))
	require.NoError(t, store.AddSession(ctx, withoutLog))

	sinkWithLog := sm.sessionElicitationSink(withLog.ID)
	sinkWithoutLog := sm.sessionElicitationSink(withoutLog.ID)
	ev := runtime.ElicitationRequest("need input", "form", nil, "", "eid-1", "", withLog.ID, nil, "root")
	sinkWithLog(ev)
	require.True(t, sm.HasEventSource(withLog.ID))

	deleted, failed := sm.BatchDeleteSessions(ctx, []string{withLog.ID, withoutLog.ID})
	assert.Equal(t, 2, deleted)
	assert.Empty(t, failed)

	sinkWithLog(ev)
	sinkWithoutLog(ev)
	assert.False(t, sm.HasEventSource(withLog.ID),
		"a stale sink invocation after BatchDeleteSessions must not resurrect the event log")
	assert.False(t, sm.HasEventSource(withoutLog.ID),
		"a stale sink invocation must not create an event log for a batch-deleted session that never had one")
}

// testConcurrentSinkVsDelete races continuous elicitation-sink invocations
// against del on a live session, then makes the deterministic check: once
// deletion has completed and every racing goroutine has stopped, one more
// stale invocation must find the tombstone and create nothing. Run with
// -race this also proves the create-vs-teardown paths are properly
// synchronised.
func testConcurrentSinkVsDelete(t *testing.T, del func(*SessionManager, *session.Session)) {
	t.Helper()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})

	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))

	sink := sm.sessionElicitationSink(sess.ID)
	ev := runtime.ElicitationRequest("need input", "form", nil, "", "eid-1", "", sess.ID, nil, "root")

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
					sink(ev)
				}
			}
		})
	}

	del(sm, sess)
	close(stop)
	wg.Wait()

	sink(ev)
	assert.False(t, sm.HasEventSource(sess.ID),
		"no sink invocation concurrent with or after deletion may leave an event log behind")
}

func TestDeleteSession_ConcurrentSinkInvocationsCannotResurrectEventLog(t *testing.T) {
	t.Parallel()

	testConcurrentSinkVsDelete(t, func(sm *SessionManager, sess *session.Session) {
		require.NoError(t, sm.DeleteSession(t.Context(), sess.ID))
	})
}

func TestBatchDeleteSessions_ConcurrentSinkInvocationsCannotResurrectEventLog(t *testing.T) {
	t.Parallel()

	testConcurrentSinkVsDelete(t, func(sm *SessionManager, sess *session.Session) {
		deleted, failed := sm.BatchDeleteSessions(t.Context(), []string{sess.ID})
		assert.Equal(t, 1, deleted)
		assert.Empty(t, failed)
	})
}

// sinkRegistrationRecordingRuntime records every OnElicitationRequest
// registration so tests can observe deletion clearing (or leaving alone) a
// runtime's elicitation sink.
type sinkRegistrationRecordingRuntime struct {
	fakeRuntime

	mu    sync.Mutex
	sinks []func(runtime.Event)
}

func (s *sinkRegistrationRecordingRuntime) OnElicitationRequest(handler func(runtime.Event)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sinks = append(s.sinks, handler)
}

func (s *sinkRegistrationRecordingRuntime) registrations() []func(runtime.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]func(runtime.Event), len(s.sinks))
	copy(out, s.sinks)
	return out
}

// TestDeleteSession_ClearsElicitationSinkOnServerOwnedRuntimeOnly verifies
// the runtime-side half of the deletion fix: DeleteSession must clear (set
// to nil) the elicitation sink of a server-owned runtime — so a later
// background elicitation hits the runtime's headless fast-decline path (see
// pkg/runtime's TestElicitationHandler_HeadlessBackgroundFastDeclines)
// instead of parking a waiter forever — while leaving an attached runtime's
// sink alone: that one is registered and owned by the embedder (pkg/app) and
// serves the embedder beyond this session's lifetime.
func TestDeleteSession_ClearsElicitationSinkOnServerOwnedRuntimeOnly(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})

	owned := session.New()
	require.NoError(t, store.AddSession(ctx, owned))
	ownedRT := &sinkRegistrationRecordingRuntime{}
	// Register the runtime the way RunSession does for a server-created
	// session: no done channel marks the entry as server-owned.
	sm.runtimeSessions.Store(owned.ID, &activeRuntimes{runtime: ownedRT, cancel: func() {}, session: owned})

	require.NoError(t, sm.DeleteSession(ctx, owned.ID))
	regs := ownedRT.registrations()
	require.Len(t, regs, 1, "DeleteSession must clear the server-registered sink")
	assert.Nil(t, regs[0], "the clearing registration must be nil so the runtime fast-declines background elicitations")

	attached := session.New()
	require.NoError(t, store.AddSession(ctx, attached))
	attachedRT := &sinkRegistrationRecordingRuntime{}
	sm.AttachRuntime(ctx, attached.ID, attachedRT, attached)

	require.NoError(t, sm.DeleteSession(ctx, attached.ID))
	assert.Empty(t, attachedRT.registrations(),
		"DeleteSession must not touch an attached runtime's embedder-owned sink")
}

// TestBatchDeleteSessions_ClearsElicitationSinkOnServerOwnedRuntimeOnly is
// the batch counterpart of the test above.
func TestBatchDeleteSessions_ClearsElicitationSinkOnServerOwnedRuntimeOnly(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})

	owned := session.New()
	attached := session.New()
	require.NoError(t, store.AddSession(ctx, owned))
	require.NoError(t, store.AddSession(ctx, attached))

	ownedRT := &sinkRegistrationRecordingRuntime{}
	sm.runtimeSessions.Store(owned.ID, &activeRuntimes{runtime: ownedRT, cancel: func() {}, session: owned})
	attachedRT := &sinkRegistrationRecordingRuntime{}
	sm.AttachRuntime(ctx, attached.ID, attachedRT, attached)

	deleted, failed := sm.BatchDeleteSessions(ctx, []string{owned.ID, attached.ID})
	assert.Equal(t, 2, deleted)
	assert.Empty(t, failed)

	regs := ownedRT.registrations()
	require.Len(t, regs, 1, "BatchDeleteSessions must clear the server-registered sink")
	assert.Nil(t, regs[0])
	assert.Empty(t, attachedRT.registrations(),
		"BatchDeleteSessions must not touch an attached runtime's embedder-owned sink")
}

// TestDeleteSession_SilencesLiveRuntimeElicitationDelivery replays the
// reviewer's #3584 deletion scenario end to end against a real
// *runtime.LocalRuntime built by runtimeForSession: after DeleteSession, an
// elicitation emission from that (still live, e.g. held by a detached
// background job) runtime must go nowhere — the sink is cleared and the
// tombstone blocks recreation — instead of resurrecting an event log for the
// deleted session.
func TestDeleteSession_SilencesLiveRuntimeElicitationDelivery(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	cfg := []byte(`agents:
  root:
    description: Test agent
    instruction: Be helpful.
    harness:
      type: claude-code
`)
	store := session.NewInMemorySessionStore()
	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))

	sources := config.Sources{"agent.yaml": config.NewBytesSource("agent.yaml", cfg)}
	sm := NewSessionManager(ctx, sources, store, 0, &config.RuntimeConfig{})

	run, _, _, err := sm.runtimeForSession(ctx, sess, "agent.yaml", "", &config.RuntimeConfig{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = run.Close() })
	lr, ok := run.(*runtime.LocalRuntime)
	require.True(t, ok, "runtimeForSession is expected to build a *runtime.LocalRuntime, got %T", run)

	// Register the runtime as RunSession would (server-owned: no done
	// channel), then delete the session out from under it.
	sm.runtimeSessions.Store(sess.ID, &activeRuntimes{runtime: run, cancel: func() {}, session: sess})
	require.NoError(t, sm.DeleteSession(ctx, sess.ID))

	lr.EmitElicitationRequestForTesting(runtime.ElicitationRequest("need input", "form", nil, "", "eid-1", "", sess.ID, nil, "root"))
	assert.False(t, sm.HasEventSource(sess.ID),
		"an elicitation raised through the deleted session's runtime must not recreate its event log")
}

// TestRegisterEventSource_AdoptsLazyEventLog covers the reverse of
// TestSessionElicitationSink_ReusesAttachedEventLog: the lazily-created log
// exists first (an out-of-band elicitation arrived before the embedder
// attached), then RegisterEventSource runs. It must adopt that log — same
// log identity, buffered events and connected listeners intact, sequence
// numbers continuing — rather than clobber it, and the registered entry's
// cancel must take over the attached-source lifecycle: stop the pump, whose
// deferred close then ends the log with session_exited.
func TestRegisterEventSource_AdoptsLazyEventLog(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sm := NewSessionManager(ctx, config.Sources{}, session.NewInMemorySessionStore(), 0, &config.RuntimeConfig{})

	ev := runtime.ElicitationRequest("need input", "form", nil, "", "eid-1", "", "bg-child", nil, "agent")
	sm.sessionElicitationSink("sess-adopt")(ev)

	lazyPE, ok := sm.eventLogs.Load("sess-adopt")
	require.True(t, ok)

	// Connect a listener before the source attaches; it must survive the
	// attachment and keep receiving on the same stream.
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()
	var mu sync.Mutex
	var received []any
	streamDone := make(chan struct{})
	go func() {
		defer close(streamDone)
		sm.StreamEvents(streamCtx, "sess-adopt", nil, func(_ uint64, event any) {
			mu.Lock()
			defer mu.Unlock()
			received = append(received, event)
		})
	}()
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(received) == 1
	}, 2*time.Second, time.Millisecond)

	pumped := make(chan struct{})
	sm.RegisterEventSource("sess-adopt", func(ctx context.Context, send func(any)) {
		send("source-event")
		close(pumped)
		<-ctx.Done()
	})
	<-pumped

	current, ok := sm.eventLogs.Load("sess-adopt")
	require.True(t, ok)
	assert.Same(t, lazyPE.log, current.log,
		"RegisterEventSource must adopt the lazily-created log, not replace it")

	seq, ok := sm.LastEventSeq("sess-adopt")
	require.True(t, ok)
	assert.Equal(t, uint64(2), seq, "the source's events must continue the adopted log's sequence")

	// Cancelling the registered entry must stop the pump; its deferred close
	// then delivers session_exited and disconnects the listener — the same
	// lifecycle a brand-new attached source has.
	current.cancel()
	select {
	case <-streamDone:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelling the adopted source must close the log and end connected streams")
	}

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, received, 3)
	assert.Same(t, ev, received[0], "the buffered out-of-band event must survive the adoption")
	assert.Equal(t, "source-event", received[1])
	exited, ok := received[2].(sessionExitedEvent)
	require.True(t, ok, "expected sessionExitedEvent, got %T", received[2])
	assert.Equal(t, "session_exited", exited.Type)
}

// --- #3584 review cycle 2: deletion vs. RegisterEventSource race ---

// testRegisterEventSourceAfterDelete pins RegisterEventSource's tombstone
// gate: registering an event source for a session already deleted via del
// must be a complete no-op — no event log stored (session IDs are never
// reused, so nobody would ever tear it down again) and no pump goroutine
// launched (src must never run). Covers both the session whose lazy log the
// deletion tore down and the one that never had a log at all.
func testRegisterEventSourceAfterDelete(t *testing.T, del func(sm *SessionManager, ids []string)) {
	t.Helper()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})

	hadLog := session.New()
	neverHadLog := session.New()
	require.NoError(t, store.AddSession(ctx, hadLog))
	require.NoError(t, store.AddSession(ctx, neverHadLog))

	// Give one session a lazily-created log so the deletion also exercises
	// the torn-down-then-register ordering, not just register-after-nothing.
	sm.sessionElicitationSink(hadLog.ID)(runtime.ElicitationRequest("need input", "form", nil, "", "eid-1", "", hadLog.ID, nil, "root"))
	require.True(t, sm.HasEventSource(hadLog.ID))

	del(sm, []string{hadLog.ID, neverHadLog.ID})
	require.False(t, sm.HasEventSource(hadLog.ID))

	started := make(chan string, 2)
	for _, id := range []string{hadLog.ID, neverHadLog.ID} {
		sm.RegisterEventSource(id, func(context.Context, func(any)) {
			started <- id
		})
		// The log entry is stored synchronously before RegisterEventSource
		// returns, so this check deterministically catches a resurrection.
		assert.False(t, sm.HasEventSource(id),
			"RegisterEventSource after deletion must not resurrect the event log for %s", id)
	}

	// A rejected registration launches no pump goroutine at all, so this
	// bounded wait never fires on correct code; on a regression it also
	// catches the pump the HasEventSource asserts above already flagged.
	select {
	case id := <-started:
		t.Fatalf("the source callback for %s must never be started for a deleted session", id)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestDeleteSession_RegisterEventSourceAfterDeleteIsNoOp(t *testing.T) {
	t.Parallel()

	testRegisterEventSourceAfterDelete(t, func(sm *SessionManager, ids []string) {
		for _, id := range ids {
			require.NoError(t, sm.DeleteSession(t.Context(), id))
		}
	})
}

func TestBatchDeleteSessions_RegisterEventSourceAfterDeleteIsNoOp(t *testing.T) {
	t.Parallel()

	testRegisterEventSourceAfterDelete(t, func(sm *SessionManager, ids []string) {
		deleted, failed := sm.BatchDeleteSessions(t.Context(), ids)
		assert.Equal(t, len(ids), deleted)
		assert.Empty(t, failed)
	})
}

// TestRegisterEventSource_RacingDeleteNeverLeavesEventLogBehind races
// RegisterEventSource against DeleteSession on the same session. eventLogsMu
// linearises the two: either the registration lands first and the deletion
// tears its log (and pump) down, or the deletion's tombstone lands first and
// the registration is rejected outright. In every interleaving, once both
// calls have returned no event log may remain, and any pump that did start
// must have been cancelled.
func TestRegisterEventSource_RacingDeleteNeverLeavesEventLogBehind(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})

	for range 50 {
		sess := session.New()
		require.NoError(t, store.AddSession(ctx, sess))

		var pumpStarted atomic.Bool
		pumpStopped := make(chan struct{}, 1)
		var delErr error
		var wg sync.WaitGroup
		wg.Go(func() {
			sm.RegisterEventSource(sess.ID, func(ctx context.Context, _ func(any)) {
				pumpStarted.Store(true)
				<-ctx.Done()
				pumpStopped <- struct{}{}
			})
		})
		wg.Go(func() {
			delErr = sm.DeleteSession(ctx, sess.ID)
		})
		wg.Wait()

		require.NoError(t, delErr)
		assert.False(t, sm.HasEventSource(sess.ID),
			"no interleaving of RegisterEventSource and DeleteSession may leave an event log behind")
		if pumpStarted.Load() {
			select {
			case <-pumpStopped:
			case <-time.After(2 * time.Second):
				t.Fatal("a pump that won the race must be cancelled by the deletion")
			}
		}
	}
}

// TestBatchDeleteSessions_ToleratesNilRuntimeCancel mirrors DeleteSession's
// nil-guard on the runtime entry's cancel: an entry registered without one
// (as newTestSessionManager does) must not panic batch deletion and must
// still be deregistered.
func TestBatchDeleteSessions_ToleratesNilRuntimeCancel(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})

	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))
	sm.runtimeSessions.Store(sess.ID, &activeRuntimes{runtime: &fakeRuntime{}, session: sess})

	deleted, failed := sm.BatchDeleteSessions(ctx, []string{sess.ID})
	assert.Equal(t, 1, deleted)
	assert.Empty(t, failed)
	_, ok := sm.runtimeSessions.Load(sess.ID)
	assert.False(t, ok, "the runtime entry must still be deregistered")
}

// stopCountingToolSet is a startable toolset that records how often it was
// stopped, so teardown tests can assert that deleting a session actually
// releases its toolset-owned resources (the stand-in for a stdio MCP
// subprocess) instead of leaking them until process exit.
type stopCountingToolSet struct {
	stops atomic.Int32
}

func (s *stopCountingToolSet) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }

func (s *stopCountingToolSet) Start(context.Context) error { return nil }

func (s *stopCountingToolSet) Stop(context.Context) error {
	s.stops.Add(1)
	return nil
}

// newToolSetSession registers a session whose runtime carries a per-session
// team with one started toolset, the shape runtimeForSession produces. It
// returns the toolset so the caller can assert on teardown.
func newToolSetSession(t *testing.T, sm *SessionManager, sess *session.Session) *stopCountingToolSet {
	t.Helper()

	ts := &stopCountingToolSet{}
	tm := team.New(team.WithAgents(agent.New("root", "", agent.WithToolSets(ts))))

	// Start the toolsets the way the runtime does; Agent.StopToolSets skips
	// any that never started, so an unstarted toolset would pass vacuously.
	agt, err := tm.AgentOrDefault("root")
	require.NoError(t, err)
	_, err = agt.Tools(t.Context())
	require.NoError(t, err)

	sm.runtimeSessions.Store(sess.ID, &activeRuntimes{
		runtime: &fakeRuntime{},
		session: sess,
		team:    tm,
	})
	return ts
}

// TestDeleteSession_StopsSessionToolSets covers the leak this fix targets: a
// per-session team's toolsets (stdio MCP subprocesses and the like) must be
// stopped once the session is deleted and its stream has drained.
func TestDeleteSession_StopsSessionToolSets(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})

	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))
	ts := newToolSetSession(t, sm, sess)

	require.NoError(t, sm.DeleteSession(ctx, sess.ID))
	require.NoError(t, sm.WaitStopped(ctx, sess.ID, 5*time.Second))

	assert.Equal(t, int32(1), ts.stops.Load(), "delete must stop the session's tool sets")
}

// TestBatchDeleteSessions_StopsSessionToolSets is the batch counterpart: the
// batch path must tear a session down exactly like DeleteSession does, and
// WaitStopped must observe that teardown rather than reporting an immediate
// false "already stopped".
func TestBatchDeleteSessions_StopsSessionToolSets(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})

	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))
	ts := newToolSetSession(t, sm, sess)

	deleted, failed := sm.BatchDeleteSessions(ctx, []string{sess.ID})
	require.Equal(t, 1, deleted)
	require.Empty(t, failed)

	require.NoError(t, sm.WaitStopped(ctx, sess.ID, 5*time.Second))
	assert.Equal(t, int32(1), ts.stops.Load(), "batch delete must stop the session's tool sets")
}

// TestWaitStopped_WaitsForToolSetTeardown pins the contract WaitStopped
// advertises: it returns only once teardown has completed, tool sets included.
// A stream still holding the streaming mutex parks it; releasing the stream
// lets teardown finish, and the toolset is stopped by the time WaitStopped
// returns.
func TestWaitStopped_WaitsForToolSetTeardown(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})

	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))
	ts := newToolSetSession(t, sm, sess)

	rs, ok := sm.runtimeSessions.Load(sess.ID)
	require.True(t, ok)
	rs.streaming.Lock() // stand in for an in-flight RunStream

	require.NoError(t, sm.DeleteSession(ctx, sess.ID))

	// Teardown is parked on the drain, so nothing has been stopped yet and a
	// short wait must time out rather than report success.
	assert.Equal(t, int32(0), ts.stops.Load())
	require.Error(t, sm.WaitStopped(ctx, sess.ID, 100*time.Millisecond))

	rs.streaming.Unlock()

	require.NoError(t, sm.WaitStopped(ctx, sess.ID, 5*time.Second))
	assert.Equal(t, int32(1), ts.stops.Load())
}

// TestDeleteSession_StopsSessionToolSetsOnlyOnce pins the idempotency the two
// delete paths rely on: a second teardown of the same session must not stop an
// already-stopped tool set again.
func TestDeleteSession_StopsSessionToolSetsOnlyOnce(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})

	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))
	ts := newToolSetSession(t, sm, sess)
	rs, ok := sm.runtimeSessions.Load(sess.ID)
	require.True(t, ok)

	require.NoError(t, sm.DeleteSession(ctx, sess.ID))
	require.NoError(t, sm.WaitStopped(ctx, sess.ID, 5*time.Second))
	require.Equal(t, int32(1), ts.stops.Load())

	// A repeated delete never reaches teardown — the session is already gone
	// from the store — so drive the second teardown directly.
	require.Error(t, sm.DeleteSession(ctx, sess.ID))
	sm.stopSessionToolSets(ctx, rs)

	assert.Equal(t, int32(1), ts.stops.Load(), "a stopped tool set must not be stopped twice")
}

type commandFakeRuntime struct {
	fakeRuntime

	mu              sync.Mutex
	currentAgent    string
	commands        types.Commands            // used when commandsByAgent lookup misses
	commandsByAgent map[string]types.Commands // per-agent tables; real multi-agent runtimes look like this
	setCalls        []string
	setErr          error
	setErrForAgent  map[string]error // per-target-agent failure; falls back to setErr
}

func (r *commandFakeRuntime) CurrentAgentName(context.Context) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.currentAgent
}

func (r *commandFakeRuntime) CurrentAgentInfo(context.Context) runtime.CurrentAgentInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	cmds := r.commands
	if per, ok := r.commandsByAgent[r.currentAgent]; ok {
		cmds = per
	}
	return runtime.CurrentAgentInfo{Name: r.currentAgent, Commands: cmds}
}

func (r *commandFakeRuntime) SetCurrentAgent(_ context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.setCalls = append(r.setCalls, name)
	if err, ok := r.setErrForAgent[name]; ok {
		return err
	}
	if r.setErr != nil {
		return r.setErr
	}
	r.currentAgent = name
	return nil
}

func (r *commandFakeRuntime) CurrentAgentTools(context.Context) ([]tools.Tool, error) {
	return nil, nil
}

func TestApplyAgentSwitchCommands_SwitchesAndStripsPrefix(t *testing.T) {
	t.Parallel()

	rt := &commandFakeRuntime{
		currentAgent: "root",
		commands: types.Commands{
			"plan": types.Command{Agent: "planner"},
		},
	}
	sm := &SessionManager{}
	messages := []api.Message{
		{Role: chat.MessageRoleUser, Content: "/plan explain X"},
	}

	require.NoError(t, sm.applyAgentSwitchCommands(t.Context(), rt, messages))

	assert.Equal(t, []string{"planner"}, rt.setCalls)
	assert.Equal(t, "planner", rt.currentAgent)
	assert.Equal(t, "explain X", messages[0].Content)
}

// TestApplyAgentSwitchCommands_SkipsSetWhenAlreadyOnTarget: callers rely on
// this idempotence to re-assert the desired agent every turn instead of
// tracking applied state themselves.
func TestApplyAgentSwitchCommands_SkipsSetWhenAlreadyOnTarget(t *testing.T) {
	t.Parallel()

	rt := &commandFakeRuntime{
		currentAgent: "planner",
		commands: types.Commands{
			"plan": types.Command{Agent: "planner"},
		},
	}
	sm := &SessionManager{}
	messages := []api.Message{
		{Role: chat.MessageRoleUser, Content: "/plan another step"},
	}

	require.NoError(t, sm.applyAgentSwitchCommands(t.Context(), rt, messages))

	assert.Empty(t, rt.setCalls)
	assert.Equal(t, "planner", rt.currentAgent)
	assert.Equal(t, "another step", messages[0].Content)
}

// TestApplyAgentSwitchCommands_LeavesInstructionCommandsAlone: rewriting
// instruction-expansion commands here would silently reinterpret text that
// existing HTTP callers may be sending as literal user input.
func TestApplyAgentSwitchCommands_LeavesInstructionCommandsAlone(t *testing.T) {
	t.Parallel()

	rt := &commandFakeRuntime{
		currentAgent: "root",
		commands: types.Commands{
			"help": types.Command{Instruction: "Give me help on ${args}"},
		},
	}
	sm := &SessionManager{}
	messages := []api.Message{
		{Role: chat.MessageRoleUser, Content: "/help me"},
	}

	require.NoError(t, sm.applyAgentSwitchCommands(t.Context(), rt, messages))

	assert.Empty(t, rt.setCalls, "instruction-expansion commands must not switch agents")
	assert.Equal(t, "root", rt.currentAgent)
	assert.Equal(t, "/help me", messages[0].Content, "content must be preserved verbatim")
}

func TestApplyAgentSwitchCommands_PassesThroughUnknownCommands(t *testing.T) {
	t.Parallel()

	rt := &commandFakeRuntime{
		currentAgent: "root",
		commands:     types.Commands{"plan": types.Command{Agent: "planner"}},
	}
	sm := &SessionManager{}
	messages := []api.Message{
		{Role: chat.MessageRoleUser, Content: "/random-command with args"},
	}

	require.NoError(t, sm.applyAgentSwitchCommands(t.Context(), rt, messages))

	assert.Empty(t, rt.setCalls)
	assert.Equal(t, "/random-command with args", messages[0].Content)
}

// TestApplyAgentSwitchCommands_IgnoresNonUserRoles: only user-authored
// input should be able to steer the active agent.
func TestApplyAgentSwitchCommands_IgnoresNonUserRoles(t *testing.T) {
	t.Parallel()

	rt := &commandFakeRuntime{
		currentAgent: "root",
		commands:     types.Commands{"plan": types.Command{Agent: "planner"}},
	}
	sm := &SessionManager{}
	messages := []api.Message{
		{Role: chat.MessageRoleAssistant, Content: "/plan should not switch"},
	}

	require.NoError(t, sm.applyAgentSwitchCommands(t.Context(), rt, messages))

	assert.Empty(t, rt.setCalls)
	assert.Equal(t, "/plan should not switch", messages[0].Content)
}

// TestApplyAgentSwitchCommands_PropagatesSetCurrentAgentError: a failed
// switch must surface so RunSession can refuse the turn instead of
// silently proceeding on the wrong agent with a rewritten message.
func TestApplyAgentSwitchCommands_PropagatesSetCurrentAgentError(t *testing.T) {
	t.Parallel()

	boom := errors.New("no such agent")
	rt := &commandFakeRuntime{
		currentAgent: "root",
		commands:     types.Commands{"plan": types.Command{Agent: "planner"}},
		setErr:       boom,
	}
	sm := &SessionManager{}
	messages := []api.Message{
		{Role: chat.MessageRoleUser, Content: "/plan explain X"},
	}

	err := sm.applyAgentSwitchCommands(t.Context(), rt, messages)
	require.ErrorIs(t, err, boom)
	assert.Equal(t, "/plan explain X", messages[0].Content, "content must be untouched when the switch failed")
}

// TestApplyAgentSwitchCommands_ResolvesUsingPreSwitchAgent: routing
// commands live on the source agent (`/plan` on root, targeting planner)
// and are not re-declared on the destination. Resolving after the switch
// would leak the prefix into history.
func TestApplyAgentSwitchCommands_ResolvesUsingPreSwitchAgent(t *testing.T) {
	t.Parallel()

	rt := &commandFakeRuntime{
		currentAgent: "root",
		commandsByAgent: map[string]types.Commands{
			"root":    {"plan": types.Command{Agent: "planner"}},
			"planner": {},
		},
	}
	sm := &SessionManager{}
	messages := []api.Message{
		{Role: chat.MessageRoleUser, Content: "/plan explain X"},
	}

	require.NoError(t, sm.applyAgentSwitchCommands(t.Context(), rt, messages))

	assert.Equal(t, []string{"planner"}, rt.setCalls)
	assert.Equal(t, "planner", rt.currentAgent)
	assert.Equal(t, "explain X", messages[0].Content)
}

// TestApplyAgentSwitchCommands_MultiBatchLookupUsesPreBatchAgent: each
// message's command lookup must consult the agent that owned the
// session at the start of the batch, not whichever agent an earlier
// switch has left the runtime on. Otherwise a batch like
// [/plan …, /build …] silently drops the second prefix when the
// target sub-agent doesn't re-declare `/build` in its own table.
func TestApplyAgentSwitchCommands_MultiBatchLookupUsesPreBatchAgent(t *testing.T) {
	t.Parallel()

	rt := &commandFakeRuntime{
		currentAgent: "root",
		commandsByAgent: map[string]types.Commands{
			"root": {
				"plan":  types.Command{Agent: "planner"},
				"build": types.Command{Agent: "root"},
			},
			"planner": {},
		},
	}
	sm := &SessionManager{}
	messages := []api.Message{
		{Role: chat.MessageRoleUser, Content: "/plan step one"},
		{Role: chat.MessageRoleUser, Content: "/build step two"},
	}

	require.NoError(t, sm.applyAgentSwitchCommands(t.Context(), rt, messages))

	assert.Equal(t, "step one", messages[0].Content)
	assert.Equal(t, "step two", messages[1].Content, "/build must be recognized against root's table even after /plan already switched the runtime")
	assert.Equal(t, "root", rt.currentAgent)
	assert.Equal(t, []string{"planner", "root"}, rt.setCalls)
}

// TestApplyAgentSwitchCommands_RollsBackOnBatchFailure: a failed switch
// mid-batch must not leave the runtime pointing at the intermediate
// agent — the caller aborts the turn, so the next request would arrive
// on the wrong agent otherwise.
func TestApplyAgentSwitchCommands_RollsBackOnBatchFailure(t *testing.T) {
	t.Parallel()

	boom := errors.New("no such agent")
	rt := &commandFakeRuntime{
		currentAgent: "root",
		commandsByAgent: map[string]types.Commands{
			"root": {
				"plan": types.Command{Agent: "planner"},
				"bad":  types.Command{Agent: "nonexistent"},
			},
			"planner": {
				"plan": types.Command{Agent: "planner"},
				"bad":  types.Command{Agent: "nonexistent"},
			},
		},
		setErrForAgent: map[string]error{"nonexistent": boom},
	}
	sm := &SessionManager{}
	messages := []api.Message{
		{Role: chat.MessageRoleUser, Content: "/plan step one"},
		{Role: chat.MessageRoleUser, Content: "/bad step two"},
	}

	err := sm.applyAgentSwitchCommands(t.Context(), rt, messages)
	require.ErrorIs(t, err, boom)
	assert.Equal(t, "root", rt.currentAgent, "runtime must be restored to the pre-batch agent")
	assert.Equal(t, []string{"planner", "nonexistent", "root"}, rt.setCalls)
}

// TestExportSessionForRecovery_IncludesErrors verifies that recorded error
// items reach the recovery export. GetAllMessages filters them out, so
// without a dedicated "errors" key a run that died with e.g. a context
// overflow would look like it just stopped mid-conversation.
func TestExportSessionForRecovery_IncludesErrors(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	sess := session.New(session.WithUserMessage("hi"))
	require.NoError(t, store.AddSession(ctx, sess))
	require.NoError(t, store.AddError(ctx, sess.ID, &session.Error{
		Message:   "the conversation has exceeded the model's context window",
		Code:      "context_exceeded",
		AgentName: "root",
	}))

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})
	export, err := sm.ExportSessionForRecovery(ctx, sess.ID)
	require.NoError(t, err)

	errs, ok := export["errors"].([]session.Error)
	require.True(t, ok, "export must carry recorded errors")
	require.Len(t, errs, 1)
	assert.Equal(t, "context_exceeded", errs[0].Code)

	messages, ok := export["messages"].([]session.Message)
	require.True(t, ok)
	require.Len(t, messages, 1, "error items must not leak into messages")
}

// TestExportSessionForRecovery_OmitsErrorsKeyWhenNone verifies the export
// shape is unchanged for sessions without recorded errors.
func TestExportSessionForRecovery_OmitsErrorsKeyWhenNone(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	sess := session.New(session.WithUserMessage("hi"))
	require.NoError(t, store.AddSession(ctx, sess))

	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})
	export, err := sm.ExportSessionForRecovery(ctx, sess.ID)
	require.NoError(t, err)

	_, present := export["errors"]
	assert.False(t, present)
}
