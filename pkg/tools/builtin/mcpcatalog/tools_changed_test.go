package mcpcatalog

import (
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestToolsChanged_FanOutToSubscribers pins the multi-runtime contract: the
// legacy SetToolsChangedHandler slot and every SubscribeToolsChanged
// subscription fire on enable/disable, and unsubscribing one runtime leaves
// the others notified.
func TestToolsChanged_FanOutToSubscribers(t *testing.T) {
	t.Parallel()
	ts := New()
	stubStartOK(ts)
	ctx := t.Context()
	id := firstOAuthServerID(t, ts)

	var legacy, first, second atomic.Int32
	ts.SetToolsChangedHandler(func() { legacy.Add(1) })
	unsubFirst := ts.SubscribeToolsChanged(func() { first.Add(1) })
	ts.SubscribeToolsChanged(func() { second.Add(1) })

	res, err := ts.handleEnable(ctx, EnableArgs{ID: id})
	require.NoError(t, err)
	require.False(t, res.IsError, "enable failed: %s", res.Output)
	assert.Equal(t, int32(1), legacy.Load())
	assert.Equal(t, int32(1), first.Load())
	assert.Equal(t, int32(1), second.Load())

	unsubFirst()
	res, err = ts.handleDisable(ctx, DisableArgs{ID: id})
	require.NoError(t, err)
	require.False(t, res.IsError, "disable failed: %s", res.Output)
	assert.Equal(t, int32(2), legacy.Load())
	assert.Equal(t, int32(1), first.Load(), "unsubscribed runtime must not be notified")
	assert.Equal(t, int32(2), second.Load())
}
