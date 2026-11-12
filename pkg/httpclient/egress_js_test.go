//go:build js

package httpclient

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWrapBrowserEgressKeepsTransportOnNodeAndUnguarded(t *testing.T) {
	assert.True(t, isNodeProcess(), "tests run under Node")

	base := &countingTransport{}
	fetch := &countingTransport{}
	assert.Same(t, base, wrapBrowserEgressWith(base, true, true, fetch), "Node keeps the dialing guarded transport")
	assert.Same(t, base, wrapBrowserEgressWith(base, false, false, fetch), "allow_private_ips is never rerouted")
	assert.Same(t, base, wrapBrowserEgress(base, true))
	assert.Same(t, base, wrapBrowserEgress(base, false))
	_, ok := newDesktopAwareTransport(true).(*desktopAwareTransport)
	assert.True(t, ok)
}

func TestWrapBrowserEgressRoutesGuardedThroughProxy(t *testing.T) {
	base := roundTripperFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("guarded dialing transport must not be used in the browser")
		return nil, nil
	})
	var seen *http.Request
	fetch := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		seen = req
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
	})
	transport := wrapBrowserEgressWith(base, true, false, fetch)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.example.com/v1", http.NoBody)
	require.NoError(t, err)
	resp, err := transport.RoundTrip(req)
	if resp != nil {
		require.NoError(t, resp.Body.Close())
	}
	require.ErrorIs(t, err, ErrBrowserEgressUnsupported)
	assert.Nil(t, seen)

	ctx, err := WithEgressProxy(t.Context(), "https://egress.example/proxy")
	require.NoError(t, err)
	resp, err = transport.RoundTrip(req.WithContext(ctx))
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.NotNil(t, seen)
	assert.Equal(t, "egress.example", seen.URL.Host)
	assert.Equal(t, "https://api.example.com/v1", seen.URL.Query().Get(EgressProxyURLParam))
}
