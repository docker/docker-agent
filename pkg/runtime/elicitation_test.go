package runtime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

func TestElicitationError_Error(t *testing.T) {
	t.Parallel()

	err := &ElicitationError{Action: "decline", Message: "user said no"}
	assert.Equal(t, "elicitation decline: user said no", err.Error())
}

func TestElicitationBridge_SendBeforeSwapReturnsError(t *testing.T) {
	t.Parallel()

	var b elicitationBridge
	err := b.send(t.Context(), Error("nothing"))
	assert.ErrorIs(t, err, errNoElicitationChannel)
}

func TestElicitationBridge_SwapReturnsPrevious(t *testing.T) {
	t.Parallel()

	var b elicitationBridge
	first := make(chan Event, 1)
	second := make(chan Event, 1)

	prev := b.swap(first)
	assert.Nil(t, prev, "first swap should return nil prev")

	prev = b.swap(second)
	assert.Equal(t, first, prev, "swap should return the previously stored channel")

	prev = b.swap(nil)
	assert.Equal(t, second, prev, "swap(nil) should return the previously stored channel")
}

func TestElicitationBridge_SendDeliversToCurrentChannel(t *testing.T) {
	t.Parallel()

	var b elicitationBridge
	ch := make(chan Event, 1)
	b.swap(ch)

	require.NoError(t, b.send(t.Context(), Error("hello")))

	select {
	case ev := <-ch:
		ee, ok := ev.(*ErrorEvent)
		require.True(t, ok)
		assert.Equal(t, "hello", ee.Error)
	case <-time.After(time.Second):
		t.Fatal("expected event, none received")
	}
}

func TestElicitationBridge_SendRecoversClosedChannel(t *testing.T) {
	t.Parallel()

	var b elicitationBridge
	ch := make(chan Event)
	b.swap(ch)
	close(ch)

	err := b.send(t.Context(), Error("closed"))
	assert.ErrorIs(t, err, errNoElicitationChannel)
}

// TestElicitationBridge_RestoreAndCloseWaitsForInflightSenders is the
// regression test for issue #3069: stream teardown must not close an event
// channel while an MCP elicitation goroutine is blocked sending to it.
//
// The test parks a send on the current channel, starts restoreAndClose, and
// verifies teardown cannot close the channel until the parked send drains.
// Running under -race exercises the close-vs-send coordination that used to
// panic with "send on closed channel".
func TestElicitationBridge_RestoreAndCloseWaitsForInflightSenders(t *testing.T) {
	t.Parallel()

	var b elicitationBridge
	current := make(chan Event)
	parent := make(chan Event, 1)
	b.swap(current)

	sendDone := make(chan error, 1)
	go func() {
		sendDone <- b.send(t.Context(), Error("inflight"))
	}()

	// Wait until the sender holds the read lock: TryLock fails only while
	// another lock is held, and the sender is the only other lock user.
	// From that point on, restoreAndClose's write lock (and therefore the
	// close) cannot possibly precede the parked send.
	require.Eventually(t, func() bool {
		if b.mu.TryLock() {
			b.mu.Unlock()
			return false
		}
		return true
	}, time.Second, time.Microsecond, "sender never acquired the read lock")

	closed := make(chan struct{})
	go func() {
		b.restoreAndClose(current, parent)
		close(closed)
	}()

	// The teardown ordering itself is verified below: the parked send must
	// complete without a send-on-closed-channel panic, which is only
	// possible if restoreAndClose waited for the read lock.

	select {
	case ev := <-current:
		ee, ok := ev.(*ErrorEvent)
		require.True(t, ok)
		assert.Equal(t, "inflight", ee.Error)
	case <-time.After(time.Second):
		t.Fatal("expected in-flight event")
	}

	select {
	case err := <-sendDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("in-flight send never completed after reader drained")
	}

	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("restoreAndClose never completed after reader drained")
	}

	select {
	case _, ok := <-current:
		assert.False(t, ok, "current channel should be closed after in-flight send completed")
	default:
		t.Fatal("current channel should be closed")
	}
}

// TestElicitationBridge_ConcurrentSendsAndCloseAreSerializedSafely runs many
// concurrent sends while closing the stream under -race to confirm the bridge
// owns all close-vs-send synchronization.
func TestElicitationBridge_ConcurrentSendsAndCloseAreSerializedSafely(t *testing.T) {
	t.Parallel()

	var b elicitationBridge
	ch := make(chan Event, 64)
	parent := make(chan Event, 1)
	b.swap(ch)

	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			for range 5 {
				_ = b.send(t.Context(), Error("x"))
			}
		})
	}

	received := make(chan struct{})
	go func() {
		defer close(received)
		for range ch {
		}
	}()

	wg.Wait()
	b.restoreAndClose(ch, parent)

	select {
	case <-received:
	case <-time.After(time.Second):
		t.Fatal("reader did not observe channel close")
	}
}

func TestLocalRuntime_FinalizeEventChannelEmitsStreamStoppedOnce(t *testing.T) {
	t.Parallel()

	rt := newElicitationTestRuntime(t)
	sess := session.New()
	events := make(chan Event, 1)
	parent := make(chan Event, 1)
	rt.elicitation.swap(events)

	rt.finalizeEventChannel(t.Context(), sess, turnEndReasonNormal, parent, events)

	var stopped int
	for ev := range events {
		if _, ok := ev.(*StreamStoppedEvent); ok {
			stopped++
		}
	}
	assert.Equal(t, 1, stopped, "StreamStopped should be emitted exactly once")
}

// TestLocalRuntime_FinalizeEventChannelDropsStreamStoppedAfterBoundedTimeout
// pins the #4136 fix's bound: the StreamStopped send waits for the
// abandoned-consumer timeout (proving it is a bounded blocking send, not the
// old plain non-blocking one) but still returns — and still drops the event
// — rather than hanging forever when nothing ever drains the buffer.
func TestLocalRuntime_FinalizeEventChannelDropsStreamStoppedAfterBoundedTimeout(t *testing.T) {
	t.Parallel()

	rt := newElicitationTestRuntime(t)
	const timeout = 50 * time.Millisecond
	rt.streamStoppedDeliveryTimeout = timeout
	sess := session.New()
	events := make(chan Event, 1)
	parent := make(chan Event, 1)
	events <- Error("buffer already full")
	rt.elicitation.swap(events)

	done := make(chan struct{})
	start := time.Now()
	go func() {
		rt.finalizeEventChannel(t.Context(), sess, turnEndReasonNormal, parent, events)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(timeout + 2*time.Second):
		t.Fatal("finalizeEventChannel deadlocked with a full buffer and no consumer")
	}
	elapsed := time.Since(start)

	assert.GreaterOrEqual(t, elapsed, timeout, "the send should wait out the full deadline before giving up")
	assert.Less(t, elapsed, timeout+2*time.Second, "finalizeEventChannel should return shortly after the deadline, not hang")

	var stopped int
	for ev := range events {
		if _, ok := ev.(*StreamStoppedEvent); ok {
			stopped++
		}
	}
	assert.Zero(t, stopped, "StreamStopped should be dropped instead of blocking forever when the buffer is full and abandoned")
}

// TestLocalRuntime_FinalizeEventChannelStreamStoppedIsLastBeforeClose pins the
// Option B decision from #3074: StreamStopped is emitted before the session-end
// hooks and telemetry run (so the UI reacts immediately), but it must still be
// the final event a consumer observes before the channel close that terminates
// its range. Session-end hooks dispatch with a nil event sink, so nothing is
// delivered onto the stream after StreamStopped and the close is the terminal
// signal. If a future change emits onto the events channel after StreamStopped
// (for example by handing the events sink to a session_end hook), this fails.
func TestLocalRuntime_FinalizeEventChannelStreamStoppedIsLastBeforeClose(t *testing.T) {
	t.Parallel()

	rt := newElicitationTestRuntime(t)
	sess := session.New()
	events := make(chan Event, defaultEventChannelCapacity)
	parent := make(chan Event, 1)

	// Seed events that stand in for the stream's prior output so asserting
	// StreamStopped is *last* is a real ordering check, not merely "it was the
	// only event delivered".
	events <- Error("prior stream output 1")
	events <- Error("prior stream output 2")
	rt.elicitation.swap(events)

	rt.finalizeEventChannel(t.Context(), sess, turnEndReasonNormal, parent, events)

	var delivered []Event
	for ev := range events {
		delivered = append(delivered, ev)
	}

	require.NotEmpty(t, delivered, "expected events delivered before the channel close")

	var stopped int
	for _, ev := range delivered {
		if _, ok := ev.(*StreamStoppedEvent); ok {
			stopped++
		}
	}
	assert.Equal(t, 1, stopped, "exactly one StreamStopped should be delivered")
	assert.IsType(t, &StreamStoppedEvent{}, delivered[len(delivered)-1],
		"StreamStopped must be the last event delivered before the channel closes")
}

// TestRunStreamClosesChannelAndRestoresElicitationOnEarlyReturn is the
// regression test for issue #3073: runStreamLoop swapped this stream's
// events channel into the elicitation bridge before registering the
// finalize defer, so the early-return paths (tool setup failure, a
// user_prompt_submit hook signalling termination) exited without closing
// the events channel or restoring the bridge. A `for range RunStream(...)`
// consumer then hung forever and the bridge kept pointing at the dead
// stream's channel.
//
// We drive the reachable early return — a user_prompt_submit hook that
// stops the run — and assert the consumer's range terminates and the
// previously-swapped elicitation channel is restored.
func TestRunStreamClosesChannelAndRestoresElicitationOnEarlyReturn(t *testing.T) {
	t.Parallel()

	const hookName = "test-stop-user-prompt-submit"
	dontContinue := false

	root := agent.New("root", "test agent",
		agent.WithModel(&mockProvider{id: "test/mock-model"}),
		agent.WithHooks(&latest.HooksConfig{
			UserPromptSubmit: []latest.HookDefinition{
				{Type: "builtin", Command: hookName},
			},
		}),
	)
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)),
		WithSessionCompaction(false),
		WithModelStore(mockModelStore{}),
	)
	require.NoError(t, err)

	require.NoError(t, rt.hooksRegistry.RegisterBuiltin(
		hookName,
		func(_ context.Context, _ *hooks.Input, _ []string) (*hooks.Output, error) {
			return &hooks.Output{Continue: &dontContinue, StopReason: "stop the run"}, nil
		},
	))

	// Seed a sentinel "parent" elicitation channel. After the stream tears
	// down, the bridge must be restored to this channel — not left pointing
	// at the stream's own (now closed) events channel.
	parent := make(chan Event, 1)
	rt.elicitation.swap(parent)

	sess := session.New(session.WithUserMessage("hi"))
	sess.Title = "Unit Test"

	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for range rt.RunStream(t.Context(), sess) {
		}
	}()

	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("RunStream consumer hung: events channel was never closed on the hook-driven early return")
	}

	restored := rt.elicitation.swap(nil)
	assert.Equal(t, parent, restored,
		"the previous elicitation channel must be restored on the early-return path")
}

func newElicitationTestRuntime(t *testing.T) *LocalRuntime {
	t.Helper()

	prov := &mockProvider{id: "test/mock-model"}
	root := agent.New("root", "test", agent.WithModel(prov))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	return rt
}
