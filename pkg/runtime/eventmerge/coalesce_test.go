package eventmerge

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
)

const window = 50 * time.Millisecond

func TestCoalesceDisabledReturnsInput(t *testing.T) {
	t.Parallel()

	in := make(chan runtime.Event)
	var recv <-chan runtime.Event = in

	assert.Equal(t, recv, Coalesce(t.Context(), in, 0))
}

func TestCoalesceForwardsFirstDeltaImmediately(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		in := make(chan runtime.Event, 8)
		out := Coalesce(t.Context(), in, window)

		first := text("Hel")
		in <- first
		assert.Same(t, first, receive(t, out))
		close(in)
		assertClosed(t, out)
	})
}

func TestCoalesceMergesDeltasWithinWindow(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		in := make(chan runtime.Event, 8)
		out := Coalesce(t.Context(), in, window)

		in <- text("a")
		receive(t, out)

		in <- text("b")
		in <- text("c")
		advance(window - time.Millisecond)
		assertNothing(t, out)

		advance(time.Millisecond)
		assert.Equal(t, "bc", receive(t, out).(*runtime.AgentChoiceEvent).Content)

		in <- text("d")
		assertNothing(t, out)
		advance(window)
		assert.Equal(t, "d", receive(t, out).(*runtime.AgentChoiceEvent).Content)

		advance(window)
		in <- text("e")
		assert.Equal(t, "e", receive(t, out).(*runtime.AgentChoiceEvent).Content, "a delta after a quiet spell is not held back")

		close(in)
		assertClosed(t, out)
	})
}

func TestCoalesceFlushesBeforeOtherEvents(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		in := make(chan runtime.Event, 8)
		out := Coalesce(t.Context(), in, window)

		in <- text("a")
		receive(t, out)

		stopped := runtime.StreamStopped("s", "root", "normal")
		in <- text("b")
		in <- text("c")
		in <- stopped
		assert.Equal(t, "bc", receive(t, out).(*runtime.AgentChoiceEvent).Content)
		assert.Same(t, stopped, receive(t, out))
		close(in)
		assertClosed(t, out)
	})
}

func TestCoalesceKeepsParallelToolCallsApart(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		in := make(chan runtime.Event, 8)
		out := Coalesce(t.Context(), in, window)

		in <- runtime.ToolCallOutput("call-1", noTool, "1a", "root")
		receive(t, out)

		in <- runtime.ToolCallOutput("call-1", noTool, "1b", "root")
		in <- runtime.ToolCallOutput("call-2", noTool, "2a", "root")
		in <- runtime.ToolCallOutput("call-2", noTool, "2b", "root")
		close(in)
		first := receive(t, out).(*runtime.ToolCallOutputEvent)
		second := receive(t, out).(*runtime.ToolCallOutputEvent)
		assert.Equal(t, "call-1", first.ToolCallID)
		assert.Equal(t, "1b", first.Output)
		assert.Equal(t, "call-2", second.ToolCallID)
		assert.Equal(t, "2a2b", second.Output)
		assertClosed(t, out)
	})
}

func TestCoalesceFlushesPendingOnClose(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		in := make(chan runtime.Event, 8)
		out := Coalesce(t.Context(), in, window)

		in <- text("a")
		receive(t, out)

		in <- text("b")
		in <- text("c")
		close(in)
		assert.Equal(t, "bc", receive(t, out).(*runtime.AgentChoiceEvent).Content)
		assertClosed(t, out)
	})
}

func TestCoalesceFlushesEarlyWhenPendingIsLarge(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		in := make(chan runtime.Event, 8)
		out := Coalesce(t.Context(), in, window)

		in <- text("a")
		receive(t, out)

		half := strings.Repeat("x", maxPendingBytes/2)
		in <- text(half)
		assertNothing(t, out)

		in <- text(half)
		assert.Len(t, receive(t, out).(*runtime.AgentChoiceEvent).Content, maxPendingBytes)
		close(in)
		assertClosed(t, out)
	})
}

func TestCoalesceDrainsInputOnceContextIsDone(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		in := make(chan runtime.Event)
		out := Coalesce(ctx, in, window)

		in <- text("never read")
		cancel()

		// With nobody reading out, the producer must still be able to finish.
		for range 3 {
			in <- text("dropped")
		}
		in <- runtime.StreamStopped("s", "root", "canceled")
		close(in)
		assertClosed(t, out)
	})
}

func TestCoalescePreservesContentAndOrder(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		script := []runtime.Event{
			runtime.StreamStarted("s", "root"),
			runtime.AgentChoiceReasoning("root", "s", "think ", "m1"),
			runtime.AgentChoiceReasoning("root", "s", "hard", "m1"),
			runtime.AgentChoice("root", "s", "Hello ", "m1"),
			runtime.AgentChoice("root", "s", "world", "m1"),
			runtime.ToolCall(tools.ToolCall{ID: "call-1"}, noTool, "root"),
			runtime.ToolCallOutput("call-1", noTool, "out ", "root"),
			runtime.ToolCallOutput("call-1", noTool, "put", "root"),
			runtime.AgentChoice("root", "s", "Done", "m2"),
			runtime.AgentChoice("root", "s", ".", "m2"),
			runtime.StreamStopped("s", "root", "normal"),
		}
		in := make(chan runtime.Event, len(script))
		out := Coalesce(t.Context(), in, window)

		var got []runtime.Event
		done := make(chan struct{})
		go func() {
			defer close(done)
			for event := range out {
				got = append(got, event)
			}
		}()
		for _, event := range script {
			in <- event
			advance(10 * time.Millisecond)
		}
		close(in)
		<-done

		assert.Equal(t, transcript(script), transcript(got))
		assert.Less(t, len(got), len(script))
	})
}

var noTool = tools.Tool{Name: "shell"}

// advance moves the synctest bubble's fake clock forward.
func advance(d time.Duration) {
	time.Sleep(d) //nolint:forbidigo // synctest bubble: advances fake time
}

func text(content string) runtime.Event {
	return runtime.AgentChoice("root", "s", content)
}

func receive(t *testing.T, out <-chan runtime.Event) runtime.Event {
	t.Helper()
	synctest.Wait()
	select {
	case event, ok := <-out:
		require.True(t, ok, "output closed early")
		return event
	default:
		require.FailNow(t, "expected an event")
		return nil
	}
}

func assertNothing(t *testing.T, out <-chan runtime.Event) {
	t.Helper()
	synctest.Wait()
	select {
	case event := <-out:
		assert.Failf(t, "unexpected event", "%#v", event)
	default:
	}
}

func assertClosed(t *testing.T, out <-chan runtime.Event) {
	t.Helper()
	synctest.Wait()
	select {
	case event, ok := <-out:
		assert.False(t, ok, "expected output to be closed, got %#v", event)
	default:
		assert.Fail(t, "expected output to be closed")
	}
}

// transcript renders a stream as type-tagged text, joining consecutive
// deltas of the same kind and target, so a stream and its merged form
// render identically.
func transcript(events []runtime.Event) []string {
	var lines []string
	var last string
	for _, event := range events {
		var key, content string
		switch ev := event.(type) {
		case *runtime.AgentChoiceEvent:
			key, content = "text "+ev.MessageID, ev.Content
		case *runtime.AgentChoiceReasoningEvent:
			key, content = "reasoning "+ev.MessageID, ev.Content
		case *runtime.ToolCallOutputEvent:
			key, content = "output "+ev.ToolCallID, ev.Output
		case *runtime.PartialToolCallEvent:
			key, content = "args "+ev.ToolCall.ID, ev.ToolCall.Function.Arguments
		default:
			lines = append(lines, fmt.Sprintf("%T", event))
			last = ""
			continue
		}
		if key == last {
			lines[len(lines)-1] += content
			continue
		}
		lines = append(lines, key+": "+content)
		last = key
	}
	return lines
}

func TestCoalesceFlushesReasoningBeforeReset(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		in := make(chan runtime.Event, 8)
		out := Coalesce(t.Context(), in, window)
		in <- runtime.AgentChoiceReasoning("root", "s", "first", "m")
		receive(t, out)
		reset := runtime.AgentChoiceReasoningReset("root", "s", "m")
		in <- runtime.AgentChoiceReasoning("root", "s", "old", "m")
		in <- runtime.AgentChoiceReasoning("root", "s", " thought", "m")
		in <- reset
		in <- runtime.AgentChoiceReasoning("root", "s", "new", "m")
		close(in)
		require.Equal(t, "old thought", receive(t, out).(*runtime.AgentChoiceReasoningEvent).Content)
		require.Same(t, reset, receive(t, out))
		require.Equal(t, "new", receive(t, out).(*runtime.AgentChoiceReasoningEvent).Content)
		assertClosed(t, out)
	})
}
