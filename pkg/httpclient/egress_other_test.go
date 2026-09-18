//go:build !js

package httpclient

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNativeTransportsIgnoreEgressProxy(t *testing.T) {
	t.Parallel()

	for _, guarded := range []bool{true, false} {
		transport := newDesktopAwareTransport(guarded)
		_, ok := transport.(*desktopAwareTransport)
		assert.True(t, ok, "guarded=%v: native transports are never wrapped", guarded)
	}

	// With a proxy in the context the guarded transport still enforces the
	// SSRF guard locally instead of rerouting to the proxy.
	ctx, err := WithEgressProxy(t.Context(), "https://egress.example/proxy")
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1/", http.NoBody)
	require.NoError(t, err)
	resp, err := NewDesktopAwareSSRFSafeTransport().RoundTrip(req)
	if resp != nil {
		require.NoError(t, resp.Body.Close())
	}
	require.ErrorContains(t, err, "non-public address")
}
