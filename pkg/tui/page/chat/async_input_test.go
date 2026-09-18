package chat

import (
	"context"
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

type delayedQueueRuntime struct {
	steerRecordingRuntime

	started chan struct{}
	release chan struct{}
	err     error
}

func (r *delayedQueueRuntime) enqueue(ctx context.Context, msg runtime.QueuedMessage, followUp bool) error {
	close(r.started)
	<-r.release
	if r.err != nil {
		return r.err
	}
	if followUp {
		return r.steerRecordingRuntime.FollowUp(ctx, msg)
	}
	return r.steerRecordingRuntime.Steer(ctx, msg)
}

func (r *delayedQueueRuntime) Steer(ctx context.Context, msg runtime.QueuedMessage) error {
	return r.enqueue(ctx, msg, false)
}

func (r *delayedQueueRuntime) FollowUp(ctx context.Context, msg runtime.QueuedMessage) error {
	return r.enqueue(ctx, msg, true)
}

func TestAsyncInputRoutesToOriginatingTab(t *testing.T) {
	t.Parallel()
	for _, follow := range []bool{false, true} {
		for _, fail := range []bool{false, true} {
			name := "steer"
			if follow {
				name = "follow-up"
			}
			if fail {
				name += "-failure"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				rt := &delayedQueueRuntime{started: make(chan struct{}), release: make(chan struct{})}
				if fail {
					rt.err = errors.New("queue rejected")
				}
				sess := session.New()
				p := New(animation.NewRuntime(), t.Context(), app.New(t.Context(), rt, sess), service.NewSessionState(sess)).(*chatPage)
				t.Cleanup(func() { Cleanup(p) })
				p.SetRoutingID("origin")
				p.working = true
				_, cmd := p.handleSendMsg(messages.SendMsg{Content: "original input", FollowUp: follow})
				done := make(chan tea.Msg, 1)
				go func() { done <- cmd() }()
				<-rt.started
				p.SetRoutingID("different")
				close(rt.release)
				result := <-done
				routed, ok := result.(messages.RoutedMsg)
				require.True(t, ok, "result must carry the original routing identity, got %T", result)
				assert.Equal(t, "origin", routed.SessionID)
				_, _ = p.Update(routed.Inner)
				if fail {
					require.Len(t, p.messageQueue, 1)
				} else {
					require.Len(t, p.pendingMessages, 1)
				}
			})
		}
	}
}

func TestAsyncInputRejectsReplacementPage(t *testing.T) {
	t.Parallel()
	for _, follow := range []bool{false, true} {
		t.Run(map[bool]string{false: "steer", true: "follow-up"}[follow], func(t *testing.T) {
			t.Parallel()
			sess := session.New()
			rt := &steerRecordingRuntime{steerErr: errors.New("rejected")}
			a := app.New(t.Context(), rt, sess)
			old := New(animation.NewRuntime(), t.Context(), a, service.NewSessionState(sess)).(*chatPage)
			old.working = true
			_, cmd := old.handleSendMsg(messages.SendMsg{Content: "old input", FollowUp: follow})
			result := cmd()
			Cleanup(old)
			replacement := New(animation.NewRuntime(), t.Context(), a, service.NewSessionState(sess)).(*chatPage)
			t.Cleanup(func() { Cleanup(replacement) })
			replacement.working = true
			_, next := replacement.Update(result)
			assert.Nil(t, next)
			assert.Empty(t, replacement.messageQueue)
			assert.Empty(t, replacement.pendingMessages)
			assert.Empty(t, rt.followedUp(), "retired page must withdraw an unconsumed submission")
		})
	}
}

func TestAsyncInputRetiredWhileEnqueueIsBlocked(t *testing.T) {
	t.Parallel()
	for _, follow := range []bool{false, true} {
		t.Run(map[bool]string{false: "steer", true: "follow-up"}[follow], func(t *testing.T) {
			t.Parallel()
			rt := &delayedQueueRuntime{started: make(chan struct{}), release: make(chan struct{})}
			sess := session.New()
			p := New(animation.NewRuntime(), t.Context(), app.New(t.Context(), rt, sess), service.NewSessionState(sess)).(*chatPage)
			p.working = true
			_, cmd := p.handleSendMsg(messages.SendMsg{Content: "old input", FollowUp: follow})
			done := make(chan tea.Msg, 1)
			go func() { done <- cmd() }()
			<-rt.started
			Cleanup(p)
			close(rt.release)
			<-done
			assert.Empty(t, rt.steered())
			assert.Empty(t, rt.followedUp())
		})
	}
}

func TestAsyncInputRetiredBeforeCommandStarts(t *testing.T) {
	t.Parallel()
	for _, follow := range []bool{false, true} {
		t.Run(map[bool]string{false: "steer", true: "follow-up"}[follow], func(t *testing.T) {
			t.Parallel()
			rt := &steerRecordingRuntime{}
			sess := session.New()
			p := New(animation.NewRuntime(), t.Context(), app.New(t.Context(), rt, sess), service.NewSessionState(sess)).(*chatPage)
			p.working = true
			_, cmd := p.handleSendMsg(messages.SendMsg{Content: "cancelled", FollowUp: follow})
			Cleanup(p)
			assert.Nil(t, cmd())
			assert.Empty(t, rt.steered())
			assert.Empty(t, rt.followedUp())
		})
	}
}

func TestStaleInputResultDoesNotRedispatchTimers(t *testing.T) {
	t.Parallel()
	sess := session.New()
	a := app.New(t.Context(), &steerRecordingRuntime{}, sess)
	old := New(animation.NewRuntime(), t.Context(), a, service.NewSessionState(sess)).(*chatPage)
	old.working = true
	_, cmd := old.handleSendMsg(messages.SendMsg{Content: "old"})
	result := cmd()
	Cleanup(old)
	p := New(animation.NewRuntime(), t.Context(), a, service.NewSessionState(sess)).(*chatPage)
	t.Cleanup(func() { Cleanup(p) })
	_, prior := p.UpdateEffects(runtime.AgentSwitching(true, "root", "child"))
	require.NotNil(t, prior.Local)
	_, effects := p.UpdateEffects(result)
	assert.Nil(t, effects.Cmd(true))
}
