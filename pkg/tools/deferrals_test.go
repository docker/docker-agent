package tools

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeferralTracker(t *testing.T) {
	var tracker DeferralTracker

	initial := tracker.Mark("session", []Tool{{Name: "read"}, {Name: "write"}})
	assert.False(t, initial[0].Deferred)
	assert.False(t, initial[1].Deferred)

	updated := tracker.MarkAt("session", "call-1", []Tool{{Name: "read"}, {Name: "write"}, {Name: "search"}})
	require.Len(t, updated, 3)
	assert.False(t, updated[0].Deferred)
	assert.False(t, updated[1].Deferred)
	assert.True(t, updated[2].Deferred)
	assert.Equal(t, "call-1", updated[2].DeferredAtToolCallID)

	stillDeferred := tracker.MarkAt("session", "call-2", []Tool{{Name: "search"}})
	assert.True(t, stillDeferred[0].Deferred)
	assert.Equal(t, "call-1", stillDeferred[0].DeferredAtToolCallID)
}

func TestDeferralTrackerScopesToolsBySession(t *testing.T) {
	var tracker DeferralTracker

	tracker.Mark("first", []Tool{{Name: "read"}})
	tracker.Mark("second", []Tool{{Name: "search"}})

	first := tracker.MarkAt("first", "call-1", []Tool{{Name: "read"}, {Name: "search"}})
	second := tracker.MarkAt("second", "call-2", []Tool{{Name: "read"}, {Name: "search"}})
	assert.False(t, first[0].Deferred)
	assert.True(t, first[1].Deferred)
	assert.True(t, second[0].Deferred)
	assert.False(t, second[1].Deferred)
}

func TestDeferralTrackerRecordsEmptyFirstCall(t *testing.T) {
	var tracker DeferralTracker

	assert.Empty(t, tracker.Mark("session", nil))
	withoutLoadPoint := tracker.Mark("session", []Tool{{Name: "read"}})
	assert.False(t, withoutLoadPoint[0].Deferred)
	marked := tracker.MarkAt("session", "call-1", []Tool{{Name: "read"}})
	assert.True(t, marked[0].Deferred)
}

func TestDeferralTrackerIgnoresUnactivatedCatalog(t *testing.T) {
	t.Parallel()
	var tracker DeferralTracker
	tracker.Mark("session", []Tool{{Name: "search_tool"}, {Name: "read", InCatalog: true, SearchOnly: true}})
	stillHidden := tracker.MarkAt("session", "search-call", []Tool{{Name: "read", InCatalog: true, SearchOnly: true}})
	assert.False(t, stillHidden[0].Deferred)
	activated := tracker.MarkAt("session", "add-call", []Tool{{Name: "read", InCatalog: true}})
	require.Len(t, activated, 1)
	assert.True(t, activated[0].Deferred)
	assert.Equal(t, "add-call", activated[0].DeferredAtToolCallID)
}
