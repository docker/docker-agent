package server

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

type drainingSessionRuntime struct {
	fakeRuntime

	started chan struct{}
	release chan struct{}
	stopped chan struct{}
}

func (r *drainingSessionRuntime) RunStream(ctx context.Context, _ *session.Session) <-chan runtime.Event {
	events := make(chan runtime.Event)
	close(r.started)
	go func() {
		defer close(events)
		defer close(r.stopped)
		<-ctx.Done()
		events <- runtime.Warning("teardown started", "root")
		<-r.release
		for range 256 {
			events <- runtime.Warning("teardown still running", "root")
		}
	}()
	return events
}

func TestRunSession_CancellationDrainsBeforeUnlock(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		rt := &drainingSessionRuntime{started: make(chan struct{}), release: make(chan struct{}), stopped: make(chan struct{})}
		sess := session.New()
		sm := newTestSessionManager(t, sess, rt)
		output, err := sm.RunSession(ctx, sess.ID, "agent", "root", []api.Message{{Content: "first"}}, "")
		require.NoError(t, err)
		outputClosed := make(chan struct{})
		go func() {
			for range output {
			}
			close(outputClosed)
		}()
		<-rt.started
		cancel()
		synctest.Wait()
		rs, ok := sm.runtimeSessions.Load(sess.ID)
		require.True(t, ok)
		unlocked := rs.streaming.TryLock()
		if unlocked {
			rs.streaming.Unlock()
		}
		assert.False(t, unlocked, "turn ownership released before runtime teardown")
		if !unlocked {
			_, err = sm.RunSession(t.Context(), sess.ID, "agent", "root", []api.Message{{Content: "second"}}, "")
			require.ErrorIs(t, err, ErrSessionBusy)
		}
		select {
		case <-outputClosed:
			t.Error("forwarder returned before runtime teardown")
		default:
		}
		close(rt.release)
		synctest.Wait()
		select {
		case <-rt.stopped:
		default:
			t.Error("teardown events were not drained")
		}
		select {
		case <-outputClosed:
		default:
			t.Error("forwarder did not finish")
		}
	})
}
