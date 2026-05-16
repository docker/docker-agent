package chatserver

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestConversationLockSet_AcquireRelease(t *testing.T) {
	l := newConversationLockSet()
	assert.True(t, l.tryAcquire("a"), "first acquire should succeed")
	assert.False(t, l.tryAcquire("a"), "second acquire on the same id should fail")
	l.release("a")
	assert.True(t, l.tryAcquire("a"), "acquire after release should succeed")
	l.release("a")
}

func TestConversationLockSet_DifferentIDsDontBlock(t *testing.T) {
	l := newConversationLockSet()
	assert.True(t, l.tryAcquire("a"))
	assert.True(t, l.tryAcquire("b"), "different ids should not block each other")
	l.release("a")
	l.release("b")
}

func TestConversationLockSet_EmptyIDIsNoop(t *testing.T) {
	l := newConversationLockSet()
	// Empty id is the "no conversation" path: tryAcquire must always
	// succeed and release must be safe.
	assert.True(t, l.tryAcquire(""))
	assert.True(t, l.tryAcquire(""))
	l.release("")
}

func TestConversationLockSet_NilIsNoop(t *testing.T) {
	var l *conversationLockSet
	assert.True(t, l.tryAcquire("a"))
	l.release("a") // must not panic
}

func TestConversationLockSet_RaceFreeUnderConcurrency(t *testing.T) {
	// Run the race detector over a hot loop. The lock set's invariant —
	// "at most one acquired ID at a time" — must hold.
	l := newConversationLockSet()
	const goroutines = 50
	const iters = 200

	var maxConcurrent atomic.Int32
	var current atomic.Int32
	var wg sync.WaitGroup
	for range goroutines {
		wg.Go(func() {
			for range iters {
				if l.tryAcquire("hot") {
					n := current.Add(1)
					if n > maxConcurrent.Load() {
						maxConcurrent.Store(n)
					}
					current.Add(-1)
					l.release("hot")
				}
			}
		})
	}
	wg.Wait()
	assert.LessOrEqual(t, maxConcurrent.Load(), int32(1),
		"at most one holder of the same id at a time")
}
