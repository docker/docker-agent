package chatserver

import (
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/session"
)

// conversationStore keeps long-lived sessions keyed by the
// `X-Conversation-Id` header so clients don't have to resend the full
// conversation history on every turn.
//
// It's an LRU with a TTL: entries past `ttl` since their last use are
// considered expired and lazily evicted on Get. When the store would
// grow past `maxEntries`, the least-recently-used entry is evicted on
// Put. Both eviction paths are O(n) since this cache is small (typical
// `maxEntries` ≤ a few hundred); a doubly-linked-list LRU would be
// strictly faster but the extra code is rarely worth it.
//
// All operations are safe for concurrent use.
type conversationStore struct {
	mu         sync.Mutex
	items      map[string]*conversationEntry
	maxEntries int
	ttl        time.Duration
	now        func() time.Time // injectable for tests
}

type conversationEntry struct {
	sess     *session.Session
	lastUsed time.Time
}

// newConversationStore returns a store that holds at most maxEntries
// sessions and forgets entries that have been idle for more than ttl.
// Either bound can be zero/negative to disable that bound. A store with
// both bounds disabled is functionally a regular map; a store with
// maxEntries == 0 is disabled and Get always misses.
func newConversationStore(maxEntries int, ttl time.Duration) *conversationStore {
	return &conversationStore{
		items:      make(map[string]*conversationEntry),
		maxEntries: maxEntries,
		ttl:        ttl,
		now:        time.Now,
	}
}

// Get returns the stored session for id and refreshes its last-used
// timestamp. Misses return nil. The store is disabled when maxEntries
// <= 0, in which case Get always misses.
func (c *conversationStore) Get(id string) *session.Session {
	if c == nil || c.maxEntries <= 0 || id == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[id]
	if !ok {
		return nil
	}
	if c.expired(e) {
		delete(c.items, id)
		return nil
	}
	e.lastUsed = c.now()
	return e.sess
}

// Put stores sess under id and evicts the least-recently-used entry if
// the store is over capacity. Has no effect when the store is disabled.
func (c *conversationStore) Put(id string, sess *session.Session) {
	if c == nil || c.maxEntries <= 0 || id == "" || sess == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.items[id] = &conversationEntry{sess: sess, lastUsed: now}

	// Drop expired neighbours in the same critical section so callers
	// don't accumulate dead weight on long-running stores.
	for k, v := range c.items {
		if c.expired(v) {
			delete(c.items, k)
		}
	}
	for len(c.items) > c.maxEntries {
		c.evictOldestLocked()
	}
}

// Delete removes id from the store, if present. Useful for clients that
// want to explicitly close out a conversation.
func (c *conversationStore) Delete(id string) {
	if c == nil || id == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, id)
}

// Len returns the current number of cached conversations. Mostly useful
// for tests and metrics.
func (c *conversationStore) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

func (c *conversationStore) expired(e *conversationEntry) bool {
	if c.ttl <= 0 {
		return false
	}
	return c.now().Sub(e.lastUsed) > c.ttl
}

// evictOldestLocked removes the oldest entry. Caller holds c.mu.
func (c *conversationStore) evictOldestLocked() {
	var oldestKey string
	var oldestTime time.Time
	first := true
	for k, v := range c.items {
		if first || v.lastUsed.Before(oldestTime) {
			oldestKey = k
			oldestTime = v.lastUsed
			first = false
		}
	}
	if !first {
		delete(c.items, oldestKey)
	}
}
