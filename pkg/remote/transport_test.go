package remote

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/desktop"
)

func TestNewTransport_UsesDesktopProxyWhenAvailable(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	// Create a transport
	transport := NewTransport(ctx)
	require.NotNil(t, transport)

	// If Docker Desktop is running, verify fallback transport is used
	if desktop.IsDockerDesktopRunning(ctx) {
		_, ok := transport.(*fallbackTransport)
		assert.True(t, ok, "transport should be *fallbackTransport when Docker Desktop is running")
	} else {
		// Otherwise, it should be a plain *http.Transport
		_, ok := transport.(*http.Transport)
		assert.True(t, ok, "transport should be *http.Transport when Docker Desktop is not running")
	}
}

func TestNewTransport_WorksWithoutDesktopProxy(t *testing.T) {
	t.Parallel()

	// Create a test server to simulate a registry
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ctx := t.Context()

	// Create a transport (should work whether Desktop is running or not)
	transport := NewTransport(ctx)
	require.NotNil(t, transport)

	// Make a simple HTTP request to verify the transport works
	client := &http.Client{Transport: transport}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, http.NoBody)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestIsProxySocketError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		errStr   string
		expected bool
	}{
		{
			name:     "no such file or directory",
			errStr:   "proxyconnect tcp: dial unix /path/to/httpproxy.sock: connect: no such file or directory",
			expected: true,
		},
		{
			name:     "connection refused",
			errStr:   "proxyconnect tcp: dial unix /path/to/httpproxy.sock: connect: connection refused",
			expected: true,
		},
		{
			name:     "proxyconnect tcp error",
			errStr:   "Post https://api.anthropic.com/v1/messages: proxyconnect tcp: some error",
			expected: true,
		},
		{
			name:     "dial unix error",
			errStr:   "dial unix /var/run/docker.sock: operation timed out",
			expected: true,
		},
		{
			name:     "regular network error",
			errStr:   "dial tcp 192.168.1.1:443: i/o timeout",
			expected: false,
		},
		{
			name:     "HTTP error",
			errStr:   "HTTP 500: internal server error",
			expected: false,
		},
		{
			name:     "nil error",
			errStr:   "",
			expected: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var err error
			if tc.errStr != "" {
				err = &testError{msg: tc.errStr}
			}
			result := isProxySocketError(err)
			assert.Equal(t, tc.expected, result)
		})
	}
}

func TestFallbackTransport_DisableCompression(t *testing.T) {
	t.Parallel()

	proxy := &http.Transport{}
	direct := &http.Transport{}

	ft := newFallbackTransport(proxy, direct)

	// Verify compression is not disabled initially
	assert.False(t, proxy.DisableCompression)
	assert.False(t, direct.DisableCompression)

	// Disable compression
	ft.DisableCompression()

	// Verify compression is now disabled on both transports
	assert.True(t, proxy.DisableCompression)
	assert.True(t, direct.DisableCompression)
}

// testError is a simple error type for testing
type testError struct {
	msg string
}

func (e *testError) Error() string {
	return e.msg
}
