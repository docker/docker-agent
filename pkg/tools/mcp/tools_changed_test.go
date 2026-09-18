package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestRefreshToolCache_NotifiesLegacyHandlerAndSubscribers pins the
// multi-runtime contract: the single SetToolsChangedHandler slot and every
// SubscribeToolsChanged subscription fire on a tool-list refresh, and an
// unsubscribed runtime stops being notified without affecting the others.
func TestRefreshToolCache_NotifiesLegacyHandlerAndSubscribers(t *testing.T) {
	t.Parallel()

	ts := newTestToolset("test", "test", &mockMCPClient{})
	ts.markStartedForTesting()

	var legacy, first, second int
	ts.SetToolsChangedHandler(func() { legacy++ })
	unsubFirst := ts.SubscribeToolsChanged(func() { first++ })
	ts.SubscribeToolsChanged(func() { second++ })

	ts.refreshToolCache(t.Context())
	unsubFirst()
	ts.refreshToolCache(t.Context())

	assert.Equal(t, 2, legacy)
	assert.Equal(t, 1, first)
	assert.Equal(t, 2, second)
}
