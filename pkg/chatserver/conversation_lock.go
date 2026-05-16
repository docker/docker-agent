package chatserver

import "sync"

// conversationLockSet ensures only one in-flight request at a time per
// conversation id. Concurrent requests sharing an id would otherwise share
// the same `*session.Session` (the cache hands out the same pointer to every
// caller for that id), and two concurrent runtime.RunStream calls on one
// session interleave message appends and produce garbled transcripts.
//
// We reject the second request with 409 Conflict instead of serialising it,
// for two reasons: it surfaces the misuse to the client immediately, and it
// keeps the handler's resource cost bounded (no queue, no waiting goroutines).
type conversationLockSet struct {
	mu     sync.Mutex
	active map[string]struct{}
}

func newConversationLockSet() *conversationLockSet {
	return &conversationLockSet{active: make(map[string]struct{})}
}

// tryAcquire returns true when id was not already in flight. The caller
// must call release when the request finishes. Empty id is a no-op (and
// returns true) so callers without a conversation id don't need a guard.
func (l *conversationLockSet) tryAcquire(id string) bool {
	if l == nil || id == "" {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.active[id]; ok {
		return false
	}
	l.active[id] = struct{}{}
	return true
}

// release marks id as no longer in flight. Safe to call when id is the
// empty string or l is nil.
func (l *conversationLockSet) release(id string) {
	if l == nil || id == "" {
		return
	}
	l.mu.Lock()
	delete(l.active, id)
	l.mu.Unlock()
}
