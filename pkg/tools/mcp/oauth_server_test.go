package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewCallbackServer_FixedPort(t *testing.T) {
	t.Parallel()

	cs, err := NewCallbackServer(3118)
	require.NoError(t, err)
	require.NotNil(t, cs)

	assert.Equal(t, "http://127.0.0.1:3118/callback", cs.GetRedirectURI())

	_ = cs.Shutdown(t.Context())
}

func TestNewCallbackServer_RandomPort(t *testing.T) {
	t.Parallel()

	cs, err := NewCallbackServer()
	require.NoError(t, err)
	require.NotNil(t, cs)

	assert.Contains(t, cs.GetRedirectURI(), "http://127.0.0.1:")
	assert.Contains(t, cs.GetRedirectURI(), "/callback")

	_ = cs.Shutdown(t.Context())
}

func TestNewCallbackServer_PortAlreadyInUse(t *testing.T) {
	t.Parallel()

	first, err := NewCallbackServer(3119)
	require.NoError(t, err)
	defer func() { _ = first.Shutdown(t.Context()) }()

	_, err = NewCallbackServer(3119)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "callback port 3119 is already in use")
}

func TestGetRedirectURI(t *testing.T) {
	t.Parallel()

	t.Run("http when tls is false", func(t *testing.T) {
		t.Parallel()

		cs, err := NewCallbackServer()
		require.NoError(t, err)
		defer func() { _ = cs.Shutdown(t.Context()) }()

		assert.Contains(t, cs.GetRedirectURI(), "http://")
		assert.Contains(t, cs.GetRedirectURI(), "/callback")
	})

	t.Run("https when tls is true", func(t *testing.T) {
		t.Parallel()

		cs, err := NewCallbackServerWithOptions(0, WithTLS())
		require.NoError(t, err)
		defer func() { _ = cs.Shutdown(t.Context()) }()

		assert.Contains(t, cs.GetRedirectURI(), "https://")
		assert.Contains(t, cs.GetRedirectURI(), "/callback")
	})
}
