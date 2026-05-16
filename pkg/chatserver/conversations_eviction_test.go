package chatserver

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

// TestConversationStore_RestoreAfterEviction tests that a conversation
// can be stored back after it's been evicted from the cache.
func TestConversationStore_RestoreAfterEviction(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	c := newConversationStore(2, time.Hour)
	c.now = func() time.Time { return now }

	// Store a conversation
	sess1 := session.New()
	sess1.AddMessage(session.UserMessage("first"))
	c.Put("conv-1", sess1)

	// Retrieve it (simulating a request starting)
	retrieved := c.Get("conv-1")
	require.NotNil(t, retrieved)
	require.Same(t, sess1, retrieved)

	// Simulate the request processing (updating the session)
	retrieved.AddMessage(session.UserMessage("updated"))

	// Manually evict the conversation (simulating LRU eviction)
	c.mu.Lock()
	delete(c.items, "conv-1")
	c.mu.Unlock()

	// Verify it's gone
	assert.Nil(t, c.Get("conv-1"), "conv-1 should be evicted")

	// Now the request completes and stores the updated session back
	// This should work even though conv-1 was evicted
	now = now.Add(time.Second)
	c.Put("conv-1", retrieved)

	// Verify the updated session is stored
	final := c.Get("conv-1")
	require.NotNil(t, final)
	assert.Same(t, retrieved, final)
	assert.Equal(t, "updated", final.GetLastUserMessageContent())
}
