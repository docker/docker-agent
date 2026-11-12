package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"testing/synctest"

	"gotest.tools/v3/assert"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

// stallingRunStream emits the trigger events, waits for the run context to be
// cancelled, then floods more events than any buffer holds before closing.
// done is closed only once every event has been consumed, so Run can only
// observe it closed if it cancelled the producer and drained the stream.
func stallingRunStream(triggers []runtime.Event, done chan<- struct{}) func(context.Context, *session.Session) <-chan runtime.Event {
	return func(ctx context.Context, _ *session.Session) <-chan runtime.Event {
		ch := make(chan runtime.Event)
		go func() {
			defer close(ch)
			defer close(done)
			for _, e := range triggers {
				ch <- e
			}
			<-ctx.Done()
			for range 256 {
				ch <- runtime.Warning("trailing", "test")
			}
		}()
		return ch
	}
}

func repeatMaxIterEvents(n int) []runtime.Event {
	events := make([]runtime.Event, n)
	for i := range events {
		events[i] = maxIterEvent(10)
	}
	return events
}

// Every early return out of a turn (error, safety cap) must cancel the
// runtime stream and drain it before the turn ends.
func TestRunEarlyReturnCancelsAndDrainsStream(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		cfg      Config
		triggers []runtime.Event
		wantErr  string
	}{
		{
			name:     "last error",
			cfg:      Config{Last: true},
			triggers: []runtime.Event{runtime.Error("model failed")},
			wantErr:  "model failed",
		},
		{
			name:     "last max iterations without auto approval",
			cfg:      Config{Last: true},
			triggers: repeatMaxIterEvents(1),
			wantErr:  "max iterations",
		},
		{
			name:     "last max iterations cap",
			cfg:      Config{Last: true, AutoApprove: true},
			triggers: repeatMaxIterEvents(maxAutoExtensions + 1),
			wantErr:  "max iterations",
		},
		{
			name:     "json error",
			cfg:      Config{OutputJSON: true},
			triggers: []runtime.Event{runtime.Error("model failed")},
			wantErr:  "model failed",
		},
		{
			name:     "json max iterations cap",
			cfg:      Config{OutputJSON: true, AutoApprove: true},
			triggers: repeatMaxIterEvents(maxAutoExtensions + 1),
		},
		{
			name:     "text max iterations cap",
			cfg:      Config{AutoApprove: true},
			triggers: repeatMaxIterEvents(maxAutoExtensions + 1),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			synctest.Test(t, func(t *testing.T) {
				done := make(chan struct{})
				rt := &mockRuntime{runStreamFn: stallingRunStream(tc.triggers, done)}

				var buf bytes.Buffer
				err := Run(t.Context(), NewPrinter(&buf), tc.cfg, rt, session.New(), []string{"hello"})
				if tc.wantErr == "" {
					assert.NilError(t, err)
				} else {
					assert.ErrorContains(t, err, tc.wantErr)
				}

				select {
				case <-done:
				default:
					t.Fatal("the turn ended before cancelling and draining the runtime stream")
				}
				assert.Check(t, !strings.Contains(buf.String(), "trailing"), "drained events must not be printed: %q", buf.String())
			})
		})
	}
}

// A turn that ends early must only cancel its own stream: later user messages
// still run with a live context.
func TestRunEarlyReturnKeepsFollowUpTurnsAlive(t *testing.T) {
	t.Parallel()

	var turns int
	var secondTurnCtxErr error
	rt := &mockRuntime{
		runStreamFn: func(ctx context.Context, _ *session.Session) <-chan runtime.Event {
			turns++
			ch := make(chan runtime.Event, maxAutoExtensions+1)
			if turns == 1 {
				for _, e := range repeatMaxIterEvents(maxAutoExtensions + 1) {
					ch <- e
				}
			} else {
				secondTurnCtxErr = ctx.Err()
				ch <- runtime.AgentChoice("test", "sess", "second turn answer")
			}
			close(ch)
			return ch
		},
	}

	var buf bytes.Buffer
	err := Run(t.Context(), NewPrinter(&buf), Config{AutoApprove: true}, rt, session.New(), []string{"one", "two"})
	assert.NilError(t, err)

	assert.Equal(t, turns, 2)
	assert.NilError(t, secondTurnCtxErr)
	assert.Check(t, strings.Contains(buf.String(), "second turn answer"), "the follow-up turn must run: %q", buf.String())
}
