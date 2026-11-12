package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestRefreshToolCache_NotifiesSubscribers pins the
// multi-runtime contract: every subscription fires on a tool-list refresh, and an
// unsubscribed runtime stops being notified without affecting the others.
func TestRefreshToolCache_NotifiesSubscribers(t *testing.T) {
	t.Parallel()

	ts := newTestToolset("test", "test", &mockMCPClient{})
	ts.markStartedForTesting()

	var first, second int
	unsubFirst := ts.SubscribeToolsChanged(func() { first++ })
	ts.SubscribeToolsChanged(func() { second++ })

	ts.refreshToolCache(t.Context())
	unsubFirst()
	ts.refreshToolCache(t.Context())
	assert.Equal(t, 1, first)
	assert.Equal(t, 2, second)
}
