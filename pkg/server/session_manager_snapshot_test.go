package server

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

type snapshotBarrierRuntime struct {
	fakeRuntime

	once    sync.Once
	reading chan struct{}
	release chan struct{}
}

func (r *snapshotBarrierRuntime) CurrentAgentName(context.Context) string {
	r.once.Do(func() {
		close(r.reading)
		<-r.release
	})
	return "root"
}

func TestGetSessionSnapshotHoldsIdleBoundaryThroughMessagesAndCursor(t *testing.T) {
	t.Parallel()
	sess := session.New()
	sess.AddMessage(&session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant, MessageID: "before", Content: "before"}})
	rt := &snapshotBarrierRuntime{reading: make(chan struct{}), release: make(chan struct{})}
	sm := newTestSessionManager(t, sess, rt)
	sm.appendSessionEvent(sess.ID, runtime.StreamStopped(sess.ID, "root", ""))
	done := make(chan struct{})
	go func() {
		defer close(done)
		snapshot, err := sm.GetSessionSnapshot(t.Context(), sess.ID)
		assert.NoError(t, err)
		if assert.NotNil(t, snapshot) {
			assert.False(t, snapshot.Streaming)
			assert.Equal(t, uint64(1), snapshot.LastEventSeq)
			if assert.Len(t, snapshot.Messages, 1) {
				assert.Equal(t, "before", snapshot.Messages[0].Message.Content)
			}
		}
	}()
	<-rt.reading
	rs, ok := sm.runtimeSessions.Load(sess.ID)
	require.True(t, ok)
	unlocked := rs.streaming.TryLock()
	if unlocked {
		rs.streaming.Unlock()
	}
	assert.False(t, unlocked, "new recall/run must not change messages or cursor during an idle snapshot")
	close(rt.release)
	<-done
	require.True(t, rs.streaming.TryLock(), "snapshot must release ownership")
	rs.streaming.Unlock()
}

func TestGetSessionSnapshotRunningCannotSupplyIdleCursor(t *testing.T) {
	t.Parallel()
	sess := session.New()
	sm := newTestSessionManager(t, sess, &fakeRuntime{})
	rs, ok := sm.runtimeSessions.Load(sess.ID)
	require.True(t, ok)
	rs.streaming.Lock()
	defer rs.streaming.Unlock()
	snapshot, err := sm.GetSessionSnapshot(t.Context(), sess.ID)
	require.NoError(t, err)
	assert.True(t, snapshot.Streaming)
}

type snapshotRecallRuntime struct {
	snapshotBarrierRuntime

	runs   chan struct{}
	steers atomic.Int32
}

func (r *snapshotRecallRuntime) RunStream(context.Context, *session.Session) <-chan runtime.Event {
	r.runs <- struct{}{}
	events := make(chan runtime.Event)
	close(events)
	return events
}

func (r *snapshotRecallRuntime) Steer(context.Context, runtime.QueuedMessage) error {
	r.steers.Add(1)
	return nil
}

func TestRecallWaitsForIdleSnapshotInsteadOfSteeringAbsentRun(t *testing.T) {
	t.Parallel()
	sess := session.New()
	rt := &snapshotRecallRuntime{snapshotBarrierRuntime: snapshotBarrierRuntime{reading: make(chan struct{}), release: make(chan struct{})}, runs: make(chan struct{}, 1)}
	sm := newTestSessionManager(t, sess, rt)
	snapshotDone := make(chan struct{})
	go func() {
		defer close(snapshotDone)
		_, err := sm.GetSessionSnapshot(t.Context(), sess.ID)
		assert.NoError(t, err)
	}()
	<-rt.reading
	recalled := make(chan error, 1)
	go func() { recalled <- sm.recallSession(t.Context(), sess.ID, runtime.QueuedMessage{Content: "wake up"}) }()
	close(rt.release)
	<-snapshotDone
	require.NoError(t, <-recalled)
	select {
	case <-rt.runs:
	case <-time.After(5 * time.Second):
		t.Fatal("idle recall never started")
	}
	require.Zero(t, rt.steers.Load())
}
