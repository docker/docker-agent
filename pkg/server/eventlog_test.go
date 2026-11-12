package server

import (
	"context"
	"slices"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// collect drains a stream into a slice until ctx is cancelled. It returns a
// function that, once the stream goroutine has exited, yields what was sent.
func collect(t *testing.T, log *eventLog, since *uint64) (stop func(), got func() []seqEvent) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())

	var mu sync.Mutex
	var events []seqEvent
	done := make(chan struct{})

	go func() {
		defer close(done)
		log.stream(ctx, since, func(seq uint64, event any) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, seqEvent{seq: seq, event: event})
		})
	}()

	stop = func() {
		cancel()
		<-done
	}
	got = func() []seqEvent {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(events)
	}
	return stop, got
}

func TestEventLog_AssignsMonotonicSequenceNumbers(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		log := newEventLog(16)

		stop, got := collect(t, log, nil)
		defer stop()

		log.append("a")
		log.append("b")
		log.append("c")

		synctest.Wait()
		require.Len(t, got(), 3)
		evs := got()
		assert.Equal(t, uint64(1), evs[0].seq)
		assert.Equal(t, uint64(2), evs[1].seq)
		assert.Equal(t, uint64(3), evs[2].seq)
		assert.Equal(t, uint64(3), log.lastSeq())
	})
}

func TestEventLog_NoSinceReplaysBufferThenTails(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		log := newEventLog(16)

		// Buffer two events before anyone connects.
		log.append("a")
		log.append("b")

		stop, got := collect(t, log, nil)
		defer stop()

		// Replay should deliver the two buffered events.
		synctest.Wait()
		require.Len(t, got(), 2)

		// And then live events tail.
		log.append("c")
		synctest.Wait()
		require.Len(t, got(), 3)
		assert.Equal(t, "c", got()[2].event)
	})
}

func TestEventLog_SinceReplaysOnlyNewerEvents(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		log := newEventLog(16)
		log.append("a") // seq 1
		log.append("b") // seq 2
		log.append("c") // seq 3

		since := uint64(2)
		stop, got := collect(t, log, &since)
		defer stop()

		synctest.Wait()
		evs := got()
		require.Len(t, evs, 1)
		assert.Equal(t, uint64(3), evs[0].seq)
		assert.Equal(t, "c", evs[0].event)
	})
}

func TestEventLog_GapMarkerWhenResumePointEvicted(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		log := newEventLog(3) // tiny ring so old events fall out
		for _, e := range []string{"a", "b", "c", "d", "e"} {
			log.append(e) // seqs 1..5; only 3,4,5 remain buffered
		}

		since := uint64(1) // wants from seq 2, but earliest buffered is 3
		stop, got := collect(t, log, &since)
		defer stop()

		synctest.Wait()
		require.NotEmpty(t, got())
		// First delivery must be the gap marker (seq 0, gapEvent).
		first := got()[0]
		assert.Equal(t, uint64(0), first.seq)
		_, isGap := first.event.(gapEvent)
		assert.True(t, isGap, "expected a gap marker, got %T", first.event)

		synctest.Wait()
		require.Len(t, got(), 4) // gap + seqs 3,4,5
		assert.Equal(t, uint64(5), got()[3].seq)
	})
}

func TestEventLog_NoGapWhenCaughtUp(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		log := newEventLog(8)
		log.append("a") // 1
		log.append("b") // 2

		since := uint64(2) // fully caught up
		stop, got := collect(t, log, &since)
		defer stop()

		log.append("c") // 3
		synctest.Wait()
		require.Len(t, got(), 1)
		evs := got()
		assert.Equal(t, uint64(3), evs[0].seq)
		// No gap marker.
		for _, e := range evs {
			_, isGap := e.event.(gapEvent)
			assert.False(t, isGap)
		}
	})
}

// TestEventLog_RegistrationIsGapless asserts the critical property: an event
// appended concurrently with a new subscriber connecting is delivered exactly
// once — either in the backlog or live, never dropped and never duplicated.
func TestEventLog_RegistrationIsGapless(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		for range 50 {
			log := newEventLog(64)
			log.append("seed") // seq 1

			// Append concurrently with a fresh (no-since) subscriber connecting.
			var wg sync.WaitGroup
			wg.Go(func() {
				log.append("racy") // seq 2
			})

			stop, got := collect(t, log, nil)
			wg.Wait()

			synctest.Wait()
			require.GreaterOrEqual(t, len(got()), 2)

			seqs := map[uint64]int{}
			for _, e := range got() {
				seqs[e.seq]++
			}
			stop()

			assert.Equal(t, 1, seqs[1], "seq 1 delivered exactly once")
			assert.Equal(t, 1, seqs[2], "seq 2 (racy) delivered exactly once")
		}
	})
}

func TestEventLog_CloseEmitsTerminalEventThenEndsStream(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		log := newEventLog(8)

		ctx := t.Context()
		var mu sync.Mutex
		var got []seqEvent
		done := make(chan struct{})
		go func() {
			defer close(done)
			log.stream(ctx, nil, func(seq uint64, event any) {
				mu.Lock()
				defer mu.Unlock()
				got = append(got, seqEvent{seq: seq, event: event})
			})
		}()

		// Let the subscriber register.
		synctest.Wait()
		require.Len(t, log.listeners, 1)

		log.close("agent exited")
		synctest.Wait()

		select {
		case <-done:
		default:
			t.Fatal("stream did not end after close")
		}

		// The client must have received a terminal session_exited event before
		// the stream ended.
		mu.Lock()
		defer mu.Unlock()
		require.NotEmpty(t, got)
		last := got[len(got)-1]
		exited, ok := last.event.(sessionExitedEvent)
		require.True(t, ok, "last event must be session_exited, got %T", last.event)
		assert.Equal(t, "agent exited", exited.Reason)
		assert.Equal(t, uint64(1), last.seq, "terminal event carries a sequence number")

		// Appending after close is a no-op and does not panic.
		log.append("ignored")
		assert.Equal(t, uint64(1), log.lastSeq())
	})
}

// TestEventLog_LateSubscriberSeesTerminalEvent verifies a client connecting
// after the session already ended still replays the session_exited marker.
func TestEventLog_LateSubscriberSeesTerminalEvent(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		log := newEventLog(8)
		log.append("a")
		log.close("done")

		stop, got := collect(t, log, nil)
		defer stop()

		synctest.Wait()
		require.Len(t, got(), 2)
		evs := got()
		_, ok := evs[len(evs)-1].event.(sessionExitedEvent)
		assert.True(t, ok, "late subscriber must replay the terminal session_exited")
	})
}

func TestEventLog_SlowSubscriberIsDropped(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		log := newEventLog(4) // ring + listener buffer of 4

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		released := make(chan struct{})
		exited := make(chan struct{})
		go func() {
			defer close(exited)
			first := true
			log.stream(ctx, nil, func(uint64, any) {
				if first {
					first = false
					<-released // stall on the very first event
				}
			})
		}()

		synctest.Wait()
		require.Len(t, log.listeners, 1)

		// Overflow the listener buffer while it is stalled.
		for i := range 20 {
			log.append(i)
		}

		// The slow listener must have been dropped.
		synctest.Wait()
		require.Empty(t, log.listeners)

		close(released)
		synctest.Wait()
		select {
		case <-exited:
		default:
			t.Fatal("dropped subscriber did not exit")
		}
	})
}
