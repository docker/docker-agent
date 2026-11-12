package acp

import (
	"context"
	"testing"
	"testing/synctest"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

type drainingPromptRuntime struct {
	fakeRuntime

	started chan struct{}
	release chan struct{}
	stopped chan struct{}
	fail    bool
}

func (r *drainingPromptRuntime) RunStream(ctx context.Context, _ *session.Session) <-chan runtime.Event {
	events := make(chan runtime.Event)
	close(r.started)
	go func() {
		defer close(events)
		defer close(r.stopped)
		if r.fail {
			events <- &runtime.ToolCallResponseEvent{ToolCallID: ""}
		}
		<-ctx.Done()
		events <- runtime.Warning("teardown started", "root")
		<-r.release
		for range 256 {
			events <- runtime.Warning("teardown still running", "root")
		}
	}()
	return events
}

func TestPrompt_DrainsBeforeReleasingTurn(t *testing.T) {
	t.Parallel()
	for _, fail := range []bool{false, true} {
		name := "cancel"
		if fail {
			name = "handler error"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				rt := &drainingPromptRuntime{started: make(chan struct{}), release: make(chan struct{}), stopped: make(chan struct{}), fail: fail}
				agent, sess, _ := newPromptTestAgent(t, rt)
				done := promptAsync(agent, t.Context(), promptRequest("first"))
				<-rt.started
				if !fail {
					require.NoError(t, agent.Cancel(t.Context(), acpsdk.CancelNotification{SessionId: testSessionID}))
				}
				synctest.Wait()
				select {
				case <-sess.turns:
					t.Error("turn token released before runtime teardown")
				default:
				}
				close(rt.release)
				synctest.Wait()
				result := <-done
				if fail {
					require.ErrorContains(t, result.err, "tool call ID is required")
				} else {
					require.NoError(t, result.err)
					assert.Equal(t, acpsdk.StopReasonCancelled, result.response.StopReason)
				}
				select {
				case <-rt.stopped:
				default:
					t.Error("teardown events were not drained")
				}
			})
		})
	}
}
