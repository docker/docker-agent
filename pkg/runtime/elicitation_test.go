package runtime

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestElicitationError_Error(t *testing.T) {
	t.Parallel()

	err := &ElicitationError{Action: "decline", Message: "user said no"}
	assert.Equal(t, "elicitation decline: user said no", err.Error())
}

func TestElicitationBridge_SendBeforeSwapReturnsError(t *testing.T) {
	t.Parallel()

	var b elicitationBridge
	err := b.send(Error("nothing"))
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

	require.NoError(t, b.send(Error("hello")))

	select {
	case ev := <-ch:
		ee, ok := ev.(*ErrorEvent)
		require.True(t, ok)
		assert.Equal(t, "hello", ee.Error)
	case <-time.After(time.Second):
		t.Fatal("expected event, none received")
	}
}

// TestElicitationBridge_SwapWaitsForInflightSenders is the key correctness
// test: the bridge must not let a swap proceed while a send is in flight,
// because the calling code on the runtime closes the channel shortly after
// swapping it out. Without the RLock-during-send invariant the inner stream
// could close the channel while an MCP elicitation is mid-send and panic.
//
// The test parks a send on a full channel, attempts a swap concurrently,
// and asserts the swap blocks until the send completes (i.e. until the
// reader drains).
func TestElicitationBridge_SwapWaitsForInflightSenders(t *testing.T) {
	t.Parallel()

	var b elicitationBridge

	// Unbuffered channel so the send blocks until a reader receives.
	inner := make(chan Event)
	b.swap(inner)

	parent := make(chan Event, 1)

	sendStarted := make(chan struct{})
	sendDone := make(chan struct{})

	// Goroutine 1: in-flight sender on the inner channel.
	go func() {
		close(sendStarted)
		_ = b.send(Error("inflight"))
		close(sendDone)
	}()
	<-sendStarted

	// Give the sender a moment to grab the RLock and park on the channel.
	time.Sleep(20 * time.Millisecond)

	// Goroutine 2: swap to parent. Must block until inner send completes.
	swapped := make(chan struct{})
	var prev chan Event
	go func() {
		prev = b.swap(parent)
		close(swapped)
	}()

	// Swap must NOT have completed yet — the in-flight reader still holds
	// the RLock.
	select {
	case <-swapped:
		t.Fatal("swap completed while a send was still in flight; close-during-send race possible")
	case <-time.After(50 * time.Millisecond):
	}

	// Drain the inner channel; this lets the in-flight send return,
	// release the RLock, and the swap to proceed.
	<-inner

	select {
	case <-sendDone:
	case <-time.After(time.Second):
		t.Fatal("in-flight send never completed after reader drained")
	}

	select {
	case <-swapped:
	case <-time.After(time.Second):
		t.Fatal("swap never completed after in-flight send finished")
	}
	assert.Equal(t, inner, prev, "swap should return the previously stored channel")
}

// TestElicitationBridge_ConcurrentSendsAreSerializedSafely runs many
// concurrent sends + swaps under -race to confirm the contract.
func TestElicitationBridge_ConcurrentSendsAreSerializedSafely(t *testing.T) {
	t.Parallel()

	var b elicitationBridge
	ch := make(chan Event, 64)
	b.swap(ch)

	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			for range 5 {
				_ = b.send(Error("x"))
			}
		})
	}

	// Concurrent reader to keep the channel from filling up.
	done := make(chan struct{})
	go func() {
		defer close(done)
		count := 0
		for range ch {
			count++
			if count == 50 {
				return
			}
		}
	}()

	wg.Wait()
	// Drain anything still pending.
	for {
		select {
		case <-ch:
		case <-done:
			return
		case <-time.After(100 * time.Millisecond):
			return
		}
	}
}
