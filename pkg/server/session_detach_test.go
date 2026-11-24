package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

// recallRuntime records the recall handler installed by AttachRuntime, so a
// detach can assert the handler was unregistered.
type recallRuntime struct {
	*fakeRuntime

	handler runtime.RecallHandler
}

func (r *recallRuntime) SetRecallHandler(handler runtime.RecallHandler) { r.handler = handler }

func newDetachTestManager(t *testing.T) (*SessionManager, *session.Session) {
	t.Helper()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))
	return NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{}), sess
}

func blockUntilCancelled(ctx context.Context, send func(any)) {
	<-ctx.Done()
}

func TestAttachRuntime_DetachCleansUpAttachedState(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sm, sess := newDetachTestManager(t)
	rt := &recallRuntime{fakeRuntime: &fakeRuntime{}}

	attachment := sm.AttachRuntime(ctx, sess.ID, rt, sess)
	require.NotNil(t, rt.handler, "attach must install the recall handler")
	sm.RegisterFollowUpInjector(sess.ID, func(context.Context, string) {})
	sm.RegisterEventSource(sess.ID, blockUntilCancelled)

	attached, ok := sm.runtimeSessions.Load(sess.ID)
	require.True(t, ok)
	_, ok = sm.eventLogs.Load(sess.ID)
	require.True(t, ok)

	attachment.Detach()

	_, ok = sm.runtimeSessions.Load(sess.ID)
	assert.False(t, ok, "detach must remove the attached runtime")
	assert.Nil(t, rt.handler, "detach must unregister the recall handler")
	_, ok = sm.followUpInjectors.Load(sess.ID)
	assert.False(t, ok, "detach must drop the follow-up injector")
	_, ok = sm.eventLogs.Load(sess.ID)
	assert.False(t, ok, "detach must drop the event log")
	select {
	case <-attached.done:
	default:
		t.Fatal("detach must cancel the attach context")
	}

	// The session itself survives the detach.
	_, err := sm.sessionStore.GetSession(ctx, sess.ID)
	require.NoError(t, err, "detach must not delete the session")

	// Attaching again works: unlike session deletion, detach does not mark the
	// event log permanently gone.
	replacement := sm.AttachRuntime(ctx, sess.ID, rt, sess)
	sm.RegisterEventSource(sess.ID, blockUntilCancelled)
	_, ok = sm.eventLogs.Load(sess.ID)
	assert.True(t, ok, "a detached session must be attachable again")
	replacement.Detach()

	// Detaching twice is a no-op.
	assert.NotPanics(t, attachment.Detach)
}

// TestDetachRuntime_KeepsBusyGuardUntilStreamStops is the regression test for
// the #3590 busy guard: closing a tab schedules cleanup without waiting for the
// tab's stream to drain, but AddMessage/UpdateMessage must keep rejecting edits
// until that stream has actually stopped.
func TestDetachRuntime_KeepsBusyGuardUntilStreamStops(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sm, sess := newDetachTestManager(t)
	rt := &recallRuntime{fakeRuntime: &fakeRuntime{}}
	attachment := sm.AttachRuntime(ctx, sess.ID, rt, sess)
	sm.RegisterFollowUpInjector(sess.ID, func(context.Context, string) {})
	sm.RegisterEventSource(sess.ID, blockUntilCancelled)

	// Hold the guard exactly like pkg/app's WithStreamGuard does around the
	// TUI's direct RunStream call. An HTTP-started stream would hold the same
	// lock while its request context keeps it alive, which is why closing the
	// tab must not drop the guard on its own.
	attachment.Lock()
	attachment.Detach()

	// The per-attachment state is gone, but the old stream still owns the busy
	// guard, so edits must keep failing.
	assert.Nil(t, rt.handler)
	assert.False(t, sm.HasEventSource(sess.ID))
	assert.Len(t, sm.GetActiveSessions(), 1, "the closing attachment stays registered while its stream drains")
	require.ErrorIs(t, sm.AddMessage(ctx, sess.ID, session.UserMessage("edit mid-stream")), ErrSessionBusy)
	require.ErrorIs(t, sm.UpdateMessage(ctx, sess.ID, "0", session.UserMessage("edit mid-stream")), ErrSessionBusy)

	// Releasing the guard ends the stream: the attachment is finally removed and
	// the stored session can be edited again.
	attachment.Unlock()

	assert.Empty(t, sm.GetActiveSessions(), "stream teardown must finish the detach")
	require.NoError(t, sm.AddMessage(ctx, sess.ID, session.UserMessage("edit after close")))
}

// TestDetachRuntime_IdleSnapshotOverlapFinishesTeardown is the regression test
// for an idle tab close that overlaps a snapshot: GetSessionSnapshot takes the
// same streaming guard for a moment on an idle attachment and releases it with
// the guard's new owner-aware release. A plain Unlock there would lose the
// teardown: finishDetach's TryLock fails during the snapshot but no stream
// goroutine ever comes back to finish it, so the closed tab would stay in
// GET /api/sessions?active=true forever (#4909 review).
func TestDetachRuntime_IdleSnapshotOverlapFinishesTeardown(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sm, sess := newDetachTestManager(t)
	rt := &recallRuntime{fakeRuntime: &fakeRuntime{}}
	attachment := sm.AttachRuntime(ctx, sess.ID, rt, sess)
	sm.RegisterFollowUpInjector(sess.ID, func(context.Context, string) {})
	sm.RegisterEventSource(sess.ID, blockUntilCancelled)

	attached, ok := sm.runtimeSessions.Load(sess.ID)
	require.True(t, ok)

	// Park the snapshot on its modelSwitch lock after it has taken the idle
	// streaming guard, so the detach below deterministically overlaps it.
	attached.modelSwitch.Lock()
	var releaseOnce sync.Once
	releaseSnapshot := func() { releaseOnce.Do(attached.modelSwitch.Unlock) }
	t.Cleanup(releaseSnapshot)

	snapshotErr := make(chan error, 1)
	go func() {
		_, err := sm.GetSessionSnapshot(ctx, sess.ID)
		snapshotErr <- err
	}()

	// Wait until the snapshot owns the guard. A failed TryLock means someone
	// holds it, and the parked snapshot is the only holder here.
	require.Eventually(t, func() bool {
		if attached.streaming.TryLock() {
			attached.streaming.Unlock()
			return false
		}
		return true
	}, 10*time.Second, time.Millisecond, "the idle snapshot must take the streaming guard")

	// Closing the tab now cannot finish: the guard is held by the snapshot, not
	// by a stream that would release it and retry the teardown.
	attachment.Detach()
	_, stillAttached := sm.runtimeSessions.Load(sess.ID)
	require.True(t, stillAttached, "teardown must wait for the snapshot's guard")
	require.Len(t, sm.GetActiveSessions(), 1, "the closing attachment stays registered while the snapshot holds its guard")

	// Releasing the snapshot guard completes the pending teardown.
	releaseSnapshot()
	require.NoError(t, <-snapshotErr)
	require.Eventually(t, func() bool {
		_, attached := sm.runtimeSessions.Load(sess.ID)
		return !attached
	}, 10*time.Second, time.Millisecond, "the snapshot's release must finish the pending detach")
	assert.Empty(t, sm.GetActiveSessions(), "a closed idle tab must not stay in /api/sessions?active=true")
}

// TestDetachRuntime_StaleCleanupLeavesReplacementUsable covers the cleanup that
// races a same-ID reattach: a detach for the old attachment must not remove the
// replacement or clear its injector, key cache, event log or recall handler.
func TestDetachRuntime_StaleCleanupLeavesReplacementUsable(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sm, sess := newDetachTestManager(t)
	oldRT := &recallRuntime{fakeRuntime: &fakeRuntime{}}
	oldAttachment := sm.AttachRuntime(ctx, sess.ID, oldRT, sess)

	// The same session ID is attached again, replacing the first attachment.
	// Reuse the same runtime instance on purpose: a stale detach used to clear
	// whatever recall handler the runtime has installed, including the
	// replacement's one.
	newAttachment := sm.AttachRuntime(ctx, sess.ID, oldRT, sess)
	sm.RegisterFollowUpInjector(sess.ID, func(context.Context, string) {})
	sm.RegisterEventSource(sess.ID, blockUntilCancelled)
	require.NotNil(t, oldRT.handler)

	// Late cleanup for the replaced attachment.
	oldAttachment.Detach()

	assert.NotNil(t, oldRT.handler, "stale cleanup must not clear the replacement's recall handler")
	assert.True(t, sm.HasEventSource(sess.ID), "stale cleanup must not drop the replacement's event log")
	_, ok := sm.followUpInjectors.Load(sess.ID)
	assert.True(t, ok, "stale cleanup must not drop the replacement's follow-up injector")
	require.Len(t, sm.GetActiveSessions(), 1)

	// The replacement is still usable and is the one that detach removes.
	require.NoError(t, sm.AddMessage(ctx, sess.ID, session.UserMessage("still editable")))
	newAttachment.Detach()
	assert.Empty(t, sm.GetActiveSessions())
}

// TestDetachRuntime_PendingTeardownDoesNotTearDownReplacement covers the other
// interleaving: the old attachment detaches while its stream is still running,
// a replacement is attached for the same ID, and only then does the old stream
// stop. Finishing the old detach must leave the replacement untouched.
func TestDetachRuntime_PendingTeardownDoesNotTearDownReplacement(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sm, sess := newDetachTestManager(t)
	oldRT := &recallRuntime{fakeRuntime: &fakeRuntime{}}
	oldAttachment := sm.AttachRuntime(ctx, sess.ID, oldRT, sess)

	oldAttachment.Lock()   // old stream is running
	oldAttachment.Detach() // cleanup arrives while the stream drains

	// A reopened tab reattaches the same session ID before the old stream ends.
	newRT := &recallRuntime{fakeRuntime: &fakeRuntime{}}
	newAttachment := sm.AttachRuntime(ctx, sess.ID, newRT, sess)
	sm.RegisterFollowUpInjector(sess.ID, func(context.Context, string) {})
	sm.RegisterEventSource(sess.ID, blockUntilCancelled)
	require.NotNil(t, newRT.handler)

	oldAttachment.Unlock() // old stream ends, finishing the pending detach

	assert.NotNil(t, newRT.handler, "finishing a stale detach must not clear the replacement's recall handler")
	assert.True(t, sm.HasEventSource(sess.ID))
	_, ok := sm.followUpInjectors.Load(sess.ID)
	assert.True(t, ok)
	require.Len(t, sm.GetActiveSessions(), 1)
	require.NoError(t, sm.AddMessage(ctx, sess.ID, session.UserMessage("still editable")))

	newAttachment.Detach()
	assert.Empty(t, sm.GetActiveSessions())
}

// TestDetachRuntime_ReattachContinuesSSECursor is the should-fix regression for
// stale SSE cursors: a subscriber that disconnects before session_exited and
// reconnects after the same session ID was reattached must keep a valid cursor,
// so it sees the replacement's startup events instead of silently filtering
// them out (and must not need a reset because of the reattach).
func TestDetachRuntime_ReattachContinuesSSECursor(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sm, sess := newDetachTestManager(t)
	rt := &recallRuntime{fakeRuntime: &fakeRuntime{}}

	first := sm.AttachRuntime(ctx, sess.ID, rt, sess)
	sm.RegisterEventSource(sess.ID, func(ctx context.Context, send func(any)) {
		send("startup-1")
		<-ctx.Done()
	})

	var cursor uint64
	require.Eventually(t, func() bool {
		seq, ok := sm.LastEventSeq(sess.ID)
		cursor = seq
		return ok && seq > 0
	}, time.Second, 5*time.Millisecond)

	first.Detach()
	require.False(t, sm.HasEventSource(sess.ID))

	second := sm.AttachRuntime(ctx, sess.ID, rt, sess)
	sm.RegisterEventSource(sess.ID, func(ctx context.Context, send func(any)) {
		send("startup-2")
		<-ctx.Done()
	})
	require.Eventually(t, func() bool {
		seq, ok := sm.LastEventSeq(sess.ID)
		return ok && seq == cursor+1
	}, time.Second, 5*time.Millisecond, "a reattached session must continue the event sequence")

	// The cursor from the old log replays the replacement's event without a gap.
	streamCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	var seqs []uint64
	require.True(t, sm.StreamEvents(streamCtx, sess.ID, &cursor, func(seq uint64, _ any) {
		seqs = append(seqs, seq)
	}))
	require.Equal(t, []uint64{cursor + 1}, seqs)
	second.Detach()
}
