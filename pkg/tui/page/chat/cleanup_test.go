package chat

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/skills"
	skillstool "github.com/docker/docker-agent/pkg/tools/builtin/skills"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

// pausedTeardownRuntime blocks like waitIfPaused, then requires the App to drain
// an unbuffered stream even after the page and its UI consumer have gone away.
type pausedTeardownRuntime struct {
	queueTestRuntime

	started chan context.Context
	drained chan struct{}
	calls   int
}

func (r *pausedTeardownRuntime) RunStream(ctx context.Context, sess *session.Session) <-chan runtime.Event {
	r.calls++
	events := make(chan runtime.Event)
	go func() {
		defer close(r.drained)
		defer close(events)
		r.started <- ctx
		<-ctx.Done()
		// Exceed the App bus buffer to catch forwarding to a departed consumer.
		for range 256 {
			events <- runtime.AgentChoice("root", sess.ID, "discarded tail")
		}
		events <- runtime.StreamStopped(sess.ID, "root", runtime.TurnEndReasonCanceled)
	}()
	return events
}

func TestCleanupCancelsPausedRunAndDrains(t *testing.T) {
	t.Parallel()

	for _, retry := range []bool{false, true} {
		t.Run(map[bool]string{false: "message", true: "retry"}[retry], func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				rt := &pausedTeardownRuntime{started: make(chan context.Context), drained: make(chan struct{})}
				sess := session.New()
				var streamGuard sync.Mutex
				a := app.New(t.Context(), rt, sess, app.WithStreamGuard(&streamGuard))
				p := New(animation.NewRuntime(), t.Context(), a, service.NewSessionState(sess)).(*chatPage)
				defer Cleanup(p)

				if retry {
					_, _ = p.handleRetry()
				} else {
					p.processMessage(messages.SendMsg{Content: "pause this run"})
				}
				defer p.msgCancel()
				runCtx := <-rt.started
				synctest.Wait()
				require.NoError(t, runCtx.Err())
				_, _ = p.handleSendMsg(messages.SendMsg{Content: "must not start", Queue: true})
				require.Len(t, p.messageQueue, 1)

				Cleanup(p)
				Cleanup(p)
				require.ErrorIs(t, runCtx.Err(), context.Canceled)
				require.ErrorIs(t, p.inputScope.ctx.Err(), context.Canceled)
				require.Nil(t, p.msgCancel)
				synctest.Wait()

				select {
				case <-rt.drained:
				default:
					t.Fatal("the canceled run must finish draining without a UI subscriber")
				}
				require.True(t, streamGuard.TryLock(), "App forwarding must release turn ownership after draining")
				streamGuard.Unlock()
				assert.Equal(t, 1, rt.calls, "teardown must not launch queued work")
				assert.Equal(t, []queuedMessage{{content: "must not start", order: 1}}, p.messageQueue)
				assert.False(t, p.streamCancelled, "teardown must not invoke the UI cancellation handler")
			})
		})
	}
}

func TestCleanupIdleAndRepeated(t *testing.T) {
	t.Parallel()

	pageCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	p := &chatPage{cancel: cancel}

	Cleanup(p)
	Cleanup(p)
	Cleanup(nil)

	require.ErrorIs(t, pageCtx.Err(), context.Canceled)
	assert.Nil(t, p.msgCancel)
	assert.NoError(t, t.Context().Err())
}

type delayedForegroundRuntime struct {
	queueTestRuntime

	skillset  *skillstool.ToolSet
	resolving chan struct{}
	release   chan struct{}
	once      sync.Once
}

func (r *delayedForegroundRuntime) CurrentAgentSkillsToolset() *skillstool.ToolSet {
	r.once.Do(func() {
		close(r.resolving)
		<-r.release
	})
	return r.skillset
}

func TestProcessMessageCapturesRunCancel(t *testing.T) {
	t.Parallel()

	for _, fork := range []bool{false, true} {
		t.Run(map[bool]string{false: "message", true: "fork"}[fork], func(t *testing.T) {
			t.Parallel()
			rt := &delayedForegroundRuntime{resolving: make(chan struct{}), release: make(chan struct{})}
			if fork {
				rt.skillset = skillstool.New([]skills.Skill{{Name: "work", Context: "fork"}}, t.TempDir())
			}
			synctest.Test(t, func(t *testing.T) {
				sess := session.New()
				a := app.New(t.Context(), rt, sess)
				p := New(animation.NewRuntime(), t.Context(), a, service.NewSessionState(sess)).(*chatPage)
				defer Cleanup(p)
				p.processMessage(messages.SendMsg{Content: "/work"})
				defer p.msgCancel()
				<-rt.resolving

				Cleanup(p)
				// Simulate another turn replacing the page's cancel slot before dispatch.
				replacementCtx, replacementCancel := context.WithCancel(t.Context())
				defer replacementCancel()
				p.msgCancel = replacementCancel
				close(rt.release)
				synctest.Wait()

				// App cancels the handle handed to Run/RunSkillFork when replacing a session.
				a.NewSession()
				require.NoError(t, replacementCtx.Err(), "the old launch must retain its own cancel function")
				synctest.Wait()
			})
		})
	}
}
