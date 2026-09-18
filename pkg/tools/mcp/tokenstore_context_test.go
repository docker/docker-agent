package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

// These tests touch the package-level default store (via resetDefaultStore)
// and therefore must not run in parallel.

const ctxStoreTestURL = "https://mcp.example.com/mcp"

func newRemoteToolsetFromContext(t *testing.T, ctx context.Context) *remoteMCPClient {
	t.Helper()
	runConfig := &config.RuntimeConfig{EnvProviderOverride: environment.NewMapEnvProvider(nil)}
	toolset := latest.Toolset{
		Type:   "mcp",
		Name:   "remote",
		Remote: latest.Remote{URL: ctxStoreTestURL, TransportType: "streamable"},
	}
	ts, err := Creator(ctx, toolset, "", runConfig, "")
	require.NoError(t, err)
	client, ok := ts.(*Toolset).mcpClient.(*remoteMCPClient)
	require.True(t, ok, "remote toolset should use remoteMCPClient")
	return client
}

func defaultStoreInitialized() bool {
	defaultStoreMu.Lock()
	defer defaultStoreMu.Unlock()
	return defaultStore != nil
}

func TestWithOAuthTokenStore_IsolatesSessionsForSameResource(t *testing.T) {
	resetDefaultStore(t)

	storeA, storeB := NewInMemoryTokenStore(), NewInMemoryTokenStore()
	clientA := newRemoteToolsetFromContext(t, WithOAuthTokenStore(t.Context(), storeA))
	clientB := newRemoteToolsetFromContext(t, WithOAuthTokenStore(t.Context(), storeB))

	require.Same(t, storeA, clientA.tokenStore)
	require.Same(t, storeB, clientB.tokenStore)

	// The oauthTransport does every token lookup, exchange and refresh, so it
	// must be bound to the injected store as well.
	_, transportA, err := clientA.createHTTPClient()
	require.NoError(t, err)
	assert.Same(t, storeA, transportA.tokenStore)

	require.NoError(t, transportA.tokenStore.StoreToken(ctxStoreTestURL, &OAuthToken{AccessToken: "a"}))
	_, err = clientB.tokenStore.GetToken(ctxStoreTestURL)
	require.Error(t, err, "session B must not see session A's token")
	tok, err := storeA.GetToken(ctxStoreTestURL)
	require.NoError(t, err)
	assert.Equal(t, "a", tok.AccessToken)

	assert.False(t, defaultStoreInitialized(), "an override must not build the process-wide store")
}

func TestWithOAuthTokenStore_SharedOverrideIsSharedWithinTeam(t *testing.T) {
	resetDefaultStore(t)

	store := NewInMemoryTokenStore()
	ctx := WithOAuthTokenStore(t.Context(), store)
	first := newRemoteToolsetFromContext(t, ctx)
	second := newRemoteToolsetFromContext(t, ctx)

	require.Same(t, store, first.tokenStore)
	require.Same(t, store, second.tokenStore)
	require.NoError(t, first.tokenStore.StoreToken(ctxStoreTestURL, &OAuthToken{AccessToken: "shared"}))
	tok, err := second.tokenStore.GetToken(ctxStoreTestURL)
	require.NoError(t, err)
	assert.Equal(t, "shared", tok.AccessToken)

	assert.False(t, defaultStoreInitialized())
}

func TestWithOAuthTokenStore_DefaultUnchanged(t *testing.T) {
	resetDefaultStore(t)

	client := newRemoteToolsetFromContext(t, t.Context())
	assert.True(t, defaultStoreInitialized(), "no override must keep building the process-wide store")
	assert.Same(t, NewKeyringTokenStore(), client.tokenStore)

	// A nil override explicitly resets to the default, and the exported
	// constructors without a ctx keep using it as well.
	viaNil := newRemoteToolsetFromContext(t, WithOAuthTokenStore(t.Context(), nil))
	assert.Same(t, NewKeyringTokenStore(), viaNil.tokenStore)
	exported := NewRemoteToolset("remote", ctxStoreTestURL, "streamable", nil, nil)
	assert.Same(t, NewKeyringTokenStore(), exported.mcpClient.(*remoteMCPClient).tokenStore)
}
