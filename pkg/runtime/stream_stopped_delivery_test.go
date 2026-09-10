package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

// slowChoiceObserver stands in for the #4136 repro's synchronous per-delta
// SQLite write: it sleeps on every AgentChoiceEvent, so the drain goroutine
// falls behind a model that streams deltas quickly and the runtime's event
// buffer is full by the time finalizeEventChannel emits StreamStopped.
type slowChoiceObserver struct{ delay time.Duration }

func (slowChoiceObserver) OnRunStart(context.Context, *session.Session) {}

func (o slowChoiceObserver) OnEvent(_ context.Context, _ *session.Session, event Event) {
	if _, ok := event.(*AgentChoiceEvent); ok {
		time.Sleep(o.delay) //nolint:forbidigo // deliberately slow consumer reproduces the #4136 back-pressure race
	}
}

// TestRunStream_StreamStoppedDeliveredUnderSlowConsumer is the deterministic
// regression test for #4136: a runtime whose event consumer can't keep up
// with the model's delta rate must still deliver exactly one StreamStopped,
// as the last event before the channel closes. Before the bounded-send fix,
// the buffer-full non-blocking emit dropped it under this exact shape (see
// the issue's validation report, table in §2.5).
func TestRunStream_StreamStoppedDeliveredUnderSlowConsumer(t *testing.T) {
	t.Parallel()

	builder := newStreamBuilder()
	const deltas = 300
	for range deltas {
		builder.AddContent("abc")
	}
	builder.AddStopWithUsage(10, 20)

	prov := &mockProvider{id: "test/mock-model", stream: builder.Build()}
	root := agent.New("root", "test agent", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	rt, err := NewLocalRuntime(t.Context(), tm,
		WithSessionCompaction(false),
		WithModelStore(mockModelStore{}),
		WithEventObserver(slowChoiceObserver{delay: time.Millisecond}),
	)
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("hi"))
	sess.Title = "Unit Test"

	var events []Event
	for ev := range rt.RunStream(t.Context(), sess) {
		events = append(events, ev)
	}

	var stoppedCount, stoppedIdx int
	for i, ev := range events {
		if _, ok := ev.(*StreamStoppedEvent); ok {
			stoppedCount++
			stoppedIdx = i
		}
	}
	require.Equal(t, 1, stoppedCount, "StreamStopped should be delivered exactly once even under a slow consumer")
	assert.Equal(t, len(events)-1, stoppedIdx, "StreamStopped must be the last event before the channel closes")
}

// TestLocalRuntime_FinalizeEventChannelDeliversStreamStoppedToSlowButAliveConsumer
// covers the middle case between "consumer keeps up" and "consumer is gone":
// the buffer is full when StreamStopped is emitted, but something reads
// before the bounded deadline elapses. The bounded send must deliver it
// rather than treat the momentarily-full buffer as an abandoned consumer.
func TestLocalRuntime_FinalizeEventChannelDeliversStreamStoppedToSlowButAliveConsumer(t *testing.T) {
	t.Parallel()

	rt := newElicitationTestRuntime(t)
	rt.streamStoppedDeliveryTimeout = 2 * time.Second
	sess := session.New()
	events := make(chan Event, 1)
	parent := make(chan Event, 1)
	events <- Error("buffer already full")
	rt.elicitation.swap(events)

	done := make(chan struct{})
	go func() {
		rt.finalizeEventChannel(t.Context(), sess, turnEndReasonNormal, parent, events)
		close(done)
	}()

	// Free the one buffered slot so the bounded send has somewhere to land,
	// simulating a consumer that is slow but still draining rather than gone.
	// This is safe regardless of whether finalizeEventChannel's send is
	// already parked on it: freeing the slot at any point before the
	// deadline lets the send land.
	received := <-events
	_, ok := received.(*ErrorEvent)
	require.True(t, ok, "expected to drain the pre-seeded event first")

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("finalizeEventChannel did not return after the consumer freed a slot")
	}

	var stopped int
	for ev := range events {
		if _, ok := ev.(*StreamStoppedEvent); ok {
			stopped++
		}
	}
	assert.Equal(t, 1, stopped, "StreamStopped should be delivered once the consumer drains a slot before the deadline")
}
