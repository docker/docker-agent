package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestRegistry() *pendingOAuthRegistry {
	return &pendingOAuthRegistry{waiters: map[string]chan<- PendingOAuthCallback{}}
}

func TestPendingOAuthRegistry_RegisterAndDeliver(t *testing.T) {
	r := newTestRegistry()
	ch := make(chan PendingOAuthCallback, 1)

	require.NoError(t, r.register("state-1", ch))

	require.NoError(t, r.deliver("state-1", PendingOAuthCallback{Code: "abc"}))

	got := <-ch
	assert.Equal(t, "abc", got.Code)
}

func TestPendingOAuthRegistry_DeliverUnknownState(t *testing.T) {
	r := newTestRegistry()
	err := r.deliver("state-nope", PendingOAuthCallback{Code: "abc"})
	require.Error(t, err)
	assert.ErrorIs(t, err, errNoWaiter)
}

func TestPendingOAuthRegistry_RegisterEmptyState(t *testing.T) {
	r := newTestRegistry()
	err := r.register("", make(chan PendingOAuthCallback, 1))
	require.Error(t, err)
	assert.ErrorIs(t, err, errEmptyState)
}

func TestPendingOAuthRegistry_RegisterDuplicate(t *testing.T) {
	r := newTestRegistry()
	require.NoError(t, r.register("state-1", make(chan PendingOAuthCallback, 1)))
	err := r.register("state-1", make(chan PendingOAuthCallback, 1))
	require.Error(t, err)
	assert.ErrorIs(t, err, errStateRegistered)
}

func TestPendingOAuthRegistry_DeliverIsOneShot(t *testing.T) {
	r := newTestRegistry()
	ch := make(chan PendingOAuthCallback, 1)
	require.NoError(t, r.register("state-1", ch))

	require.NoError(t, r.deliver("state-1", PendingOAuthCallback{Code: "first"}))
	// Second deliver returns errNoWaiter because the entry was consumed.
	err := r.deliver("state-1", PendingOAuthCallback{Code: "second"})
	require.Error(t, err)
	assert.ErrorIs(t, err, errNoWaiter)
}

func TestPendingOAuthRegistry_UnregisterRemovesEntry(t *testing.T) {
	r := newTestRegistry()
	require.NoError(t, r.register("state-1", make(chan PendingOAuthCallback, 1)))
	r.unregister("state-1")
	err := r.deliver("state-1", PendingOAuthCallback{Code: "abc"})
	require.Error(t, err)
	assert.ErrorIs(t, err, errNoWaiter)
}
