package server

import (
	"context"
	"testing"

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

func TestDetachRuntime_CleansUpAttachedState(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	sm := NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{})
	sess := session.New()
	require.NoError(t, store.AddSession(ctx, sess))
	rt := &recallRuntime{fakeRuntime: &fakeRuntime{}}

	sm.AttachRuntime(ctx, sess.ID, rt, sess)
	require.NotNil(t, rt.handler, "attach must install the recall handler")
	sm.RegisterFollowUpInjector(sess.ID, func(context.Context, string) {})
	sm.RegisterEventSource(sess.ID, func(context.Context, func(any)) {})

	attached, ok := sm.runtimeSessions.Load(sess.ID)
	require.True(t, ok)
	_, ok = sm.eventLogs.Load(sess.ID)
	require.True(t, ok)

	sm.DetachRuntime(sess.ID)

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
	_, err := store.GetSession(ctx, sess.ID)
	require.NoError(t, err, "detach must not delete the session")

	// Attaching again works: unlike session deletion, detach does not mark the
	// event log permanently gone.
	sm.AttachRuntime(ctx, sess.ID, rt, sess)
	sm.RegisterEventSource(sess.ID, func(context.Context, func(any)) {})
	_, ok = sm.eventLogs.Load(sess.ID)
	assert.True(t, ok, "a detached session must be attachable again")
}

func TestDetachRuntime_UnknownSessionIsNoop(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	sm := NewSessionManager(ctx, config.Sources{}, session.NewInMemorySessionStore(), 0, &config.RuntimeConfig{})

	assert.NotPanics(t, func() { sm.DetachRuntime("missing") })
}
