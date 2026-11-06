package server

import (
	"context"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/runtime/remote"
	"github.com/docker/docker-agent/pkg/session"
)

func TestRecallSession_CreatesReplayableEventLogBeforeFirstEvent(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		sess := session.New()
		events := []runtime.Event{runtime.StreamStarted(sess.ID, "root"), runtime.Warning("recall answer", "root"), runtime.StreamStopped(sess.ID, "root", "")}
		fake := &scriptedStreamRuntime{events: events}
		sm := newTestSessionManager(t, sess, fake)
		require.False(t, sm.HasEventSource(sess.ID))
		require.NoError(t, sm.recallSession(t.Context(), sess.ID, runtime.QueuedMessage{Content: "wake up"}))
		require.True(t, sm.HasEventSource(sess.ID), "GET /events must be available as soon as recall starts")
		waitSessionIdle(t, sm, sess.ID)
		seq, ok := sm.LastEventSeq(sess.ID)
		require.True(t, ok)
		assert.Equal(t, uint64(len(events)), seq)
		got := replaySessionEvents(t, sm, sess.ID, len(events))
		for i := range events {
			assert.Same(t, events[i], got[i])
		}
		require.NoError(t, sm.DeleteSession(t.Context(), sess.ID))
		require.False(t, sm.HasEventSource(sess.ID))
		sm.appendSessionEvent(sess.ID, runtime.Warning("late event", "root"))
		assert.False(t, sm.HasEventSource(sess.ID), "late recalls must not resurrect deleted logs")
	})
}

func TestRecallSession_RemoteRuntimeReceivesIdleRecallAfterTurnCancellation(t *testing.T) {
	t.Parallel()

	sess := session.New()
	fake := &scriptedStreamRuntime{events: []runtime.Event{runtime.Warning("answer", "root")}}
	sm := newTestSessionManager(t, sess, fake)
	srv := httptest.NewServer(NewWithManager(sm, "").e)
	t.Cleanup(srv.Close)
	client, err := remote.NewClient(srv.URL)
	require.NoError(t, err)
	rt, err := remote.NewRuntime(client)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	background := make(chan runtime.Event, 4)
	rt.OnBackgroundEvent(func(event runtime.Event) { background <- event })

	turnCtx, cancel := context.WithCancel(t.Context())
	for range rt.RunStream(turnCtx, &session.Session{ID: sess.ID}) {
	}
	cancel()
	require.False(t, sm.HasEventSource(sess.ID), "ordinary RunSession must not duplicate its foreground events in the log")
	require.NoError(t, sm.recallSession(t.Context(), sess.ID, runtime.QueuedMessage{Content: "wake up"}))
	select {
	case event := <-background:
		warning, ok := event.(*runtime.WarningEvent)
		require.True(t, ok, "got %T", event)
		assert.Equal(t, "answer", warning.Message)
	case <-time.After(3 * time.Second):
		t.Fatal("remote runtime missed server-owned idle recall")
	}
	assert.Empty(t, background, "foreground answer must not be delivered again")
}
