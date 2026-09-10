package httpclient

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewAllowPrivateIPsClientReachesLoopback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, http.NoBody)
	require.NoError(t, err)
	response, err := NewAllowPrivateIPsClient(time.Second).Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusNoContent, response.StatusCode)
}

func TestClientForAllowPrivateIPsSharesPolicyTransport(t *testing.T) {
	safeFirst := ClientForAllowPrivateIPs(time.Second, false)
	safeSecond := ClientForAllowPrivateIPs(2*time.Second, false)
	privateClient := ClientForAllowPrivateIPs(3*time.Second, true)

	require.NotSame(t, safeFirst, safeSecond)
	assert.Equal(t, time.Second, safeFirst.Timeout)
	assert.Equal(t, 2*time.Second, safeSecond.Timeout)
	assert.Same(t, safeFirst.Transport, safeSecond.Transport)
	assert.Same(t, safeFirst.Transport, TransportForAllowPrivateIPs(false))
	assert.Same(t, privateClient.Transport, TransportForAllowPrivateIPs(true))
	assert.NotSame(t, safeFirst.Transport, privateClient.Transport)
}

func TestNewSafeClientRejectsLoopback(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, http.NoBody)
	require.NoError(t, err)
	response, err := NewSafeClient(time.Second, false).Do(request)
	if response != nil {
		defer response.Body.Close()
	}
	require.Error(t, err)
}
