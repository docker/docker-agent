package chat

import (
	"context"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

type cancellablePageRuntime struct {
	queueTestRuntime

	started chan context.Context
}

func (r *cancellablePageRuntime) RunStream(ctx context.Context, _ *session.Session) <-chan runtime.Event {
	r.started <- ctx
	ch := make(chan runtime.Event)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch
}

func TestHiddenEffectsPreserveQueueProgressionAndCancellation(t *testing.T) {
	t.Parallel()
	rt := &cancellablePageRuntime{started: make(chan context.Context, 1)}
	sess := session.New()
	p := New(animation.NewRuntime(), t.Context(), app.New(t.Context(), rt, sess), service.NewSessionState(sess)).(*chatPage)
	t.Cleanup(func() { Cleanup(p) })
	p.SetInterruptMode(msgtypes.InterruptModeNone)
	_, _ = p.UpdateEffects(runtime.StreamStarted(sess.ID, "root"))
	_, _ = p.UpdateEffects(msgtypes.SendMsg{Content: "next turn", Queue: true})
	require.Equal(t, 1, p.QueueLength())

	_, effects := p.UpdateEffects(runtime.StreamStopped(sess.ID, "root", "normal"))
	assert.Nil(t, effects.Cmd(false), "queue execution does not depend on visible commands")
	assert.Zero(t, p.QueueLength())
	var runCtx context.Context
	select {
	case runCtx = <-rt.started:
	case <-time.After(5 * time.Second):
		t.Fatal("queued turn did not start in the background")
	}
	assert.True(t, p.IsWorking())

	_, _ = p.UpdateEffects(runtime.StreamStarted(sess.ID, "root"))
	_, _ = p.UpdateEffects(runtime.StreamStarted("child-session", "child"))
	_, nested := p.UpdateEffects(runtime.StreamStopped("child-session", "child", "normal"))
	assert.Nil(t, nested.Cmd(false))
	require.NotNil(t, p.msgCancel, "a child stop must leave the parent cancellable")
	require.NoError(t, runCtx.Err())

	_, _ = p.UpdateEffects(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.ErrorIs(t, runCtx.Err(), context.Canceled, "cancellation happens in Update, not in a discarded UI command")
	assert.False(t, p.IsWorking())
}
