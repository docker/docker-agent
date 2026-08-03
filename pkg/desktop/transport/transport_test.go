package transport

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDesktopRunningOverrideBypassesMemoizedDetection(t *testing.T) {
	desktopRunningOverrideMu.Lock()
	previousDesktopRunning := desktopRunning
	desktopRunning = func(context.Context) (bool, error) { return false, nil }
	desktopRunningOverride = nil
	desktopRunningOverrideMu.Unlock()
	resetDesktopDetectionForTest()
	t.Cleanup(func() {
		desktopRunningOverrideMu.Lock()
		desktopRunning = previousDesktopRunning
		desktopRunningOverride = nil
		desktopRunningOverrideMu.Unlock()
		resetDesktopDetectionForTest()
	})

	running, err := DesktopRunning(t.Context())
	require.NoError(t, err)
	assert.False(t, running)

	calls := 0
	t.Cleanup(SetDesktopRunningForTest(func(context.Context) (bool, error) {
		calls++
		return true, nil
	}))

	for range 2 {
		running, err = DesktopRunning(t.Context())
		require.NoError(t, err)
		assert.True(t, running)
	}
	assert.Equal(t, 2, calls)
}

func TestDesktopRunningRefreshesStaleValueWithoutBlocking(t *testing.T) {
	refresh := make(chan struct{})
	startedRefresh := make(chan struct{})
	refreshDone := make(chan struct{})
	desktopRunningOverrideMu.Lock()
	previous := desktopRunning
	desktopRunning = func(context.Context) (bool, error) {
		close(startedRefresh)
		<-refresh
		close(refreshDone)
		return true, nil
	}
	desktopRunningOverrideMu.Unlock()
	resetDesktopDetectionForTest()
	desktopDetection.mu.Lock()
	desktopDetection.value = false
	desktopDetection.hasValue = true
	desktopDetection.expires = time.Now().Add(-time.Second)
	desktopDetection.mu.Unlock()
	t.Cleanup(func() {
		close(refresh)
		<-refreshDone
		desktopRunningOverrideMu.Lock()
		desktopRunning = previous
		desktopRunningOverrideMu.Unlock()
		resetDesktopDetectionForTest()
	})

	started := make(chan struct{})
	go func() {
		_, _ = DesktopRunning(t.Context())
		close(started)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("stale desktop detection blocked")
	}

	select {
	case <-startedRefresh:
	case <-time.After(time.Second):
		t.Fatal("stale desktop detection did not refresh")
	}

	running, err := DesktopRunning(t.Context())
	require.NoError(t, err)
	assert.False(t, running)
}

func TestResetDesktopDetectionForTestDiscardsInFlightDetection(t *testing.T) {
	firstStarted := make(chan struct{})
	firstRelease := make(chan struct{})
	firstDone := make(chan struct{})
	var calls atomic.Int32

	desktopRunningOverrideMu.Lock()
	previous := desktopRunning
	desktopRunning = func(context.Context) (bool, error) {
		if calls.Add(1) == 1 {
			close(firstStarted)
			<-firstRelease
			close(firstDone)
			return true, errors.New("stale detection error")
		}
		return false, nil
	}
	desktopRunningOverrideMu.Unlock()
	resetDesktopDetectionForTest()
	t.Cleanup(func() {
		if firstRelease != nil {
			close(firstRelease)
		}
		if firstDone != nil {
			<-firstDone
		}
		desktopRunningOverrideMu.Lock()
		desktopRunning = previous
		desktopRunningOverrideMu.Unlock()
		resetDesktopDetectionForTest()
	})

	result := make(chan struct {
		running bool
		err     error
	}, 1)
	go func() {
		running, err := DesktopRunning(t.Context())
		result <- struct {
			running bool
			err     error
		}{running, err}
	}()

	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("desktop detection did not start")
	}

	resetDesktopDetectionForTest()

	select {
	case result := <-result:
		require.NoError(t, result.err)
		assert.False(t, result.running)
	case <-time.After(time.Second):
		t.Fatal("desktop detection remained blocked after reset")
	}

	close(firstRelease)
	<-firstDone
	firstRelease = nil
	firstDone = nil

	running, err := DesktopRunning(t.Context())
	require.NoError(t, err)
	assert.False(t, running)
	assert.Equal(t, int32(2), calls.Load())
}

func TestNew_UsesDesktopProxyWhenAvailable(t *testing.T) {
	t.Cleanup(SetDesktopRunningForTest(func(context.Context) (bool, error) {
		return true, nil
	}))

	rt := New(t.Context())
	require.IsType(t, &fallbackTransport{}, rt)
}

func TestNew_PreservesWrappedDefaultTransport(t *testing.T) {
	// Intentionally not parallel: mutates the http.DefaultTransport global.
	previous := http.DefaultTransport
	wrapped := &countingRoundTripper{}
	http.DefaultTransport = wrapped
	t.Cleanup(func() { http.DefaultTransport = previous })

	for _, test := range []struct {
		name    string
		running bool
	}{
		{name: "without Desktop", running: false},
		{name: "with Desktop", running: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Cleanup(SetDesktopRunningForTest(func(context.Context) (bool, error) {
				return test.running, nil
			}))

			rt := New(t.Context())
			assert.Same(t, wrapped, rt)

			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com/", http.NoBody)
			require.NoError(t, err)
			resp, err := (&http.Client{Transport: rt}).Do(req)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
		})
	}

	assert.Equal(t, int32(2), wrapped.calls.Load())
}

func TestNewWithDirectTransportPreservesHTTPTransportAcrossDesktopAvailability(t *testing.T) {
	for _, test := range []struct {
		name    string
		running bool
	}{
		{name: "without Desktop", running: false},
		{name: "with Desktop", running: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			direct := &http.Transport{}
			t.Cleanup(SetDesktopRunningForTest(func(context.Context) (bool, error) {
				return test.running, nil
			}))

			rt := NewWithDirectTransport(t.Context(), direct)
			if !test.running {
				assert.Same(t, direct, rt)
				return
			}

			fallback, ok := rt.(*fallbackTransport)
			require.True(t, ok)
			assert.Same(t, direct, fallback.direct)
		})
	}
}

func TestNew_WorksWithoutDesktopProxy(t *testing.T) {
	t.Cleanup(SetDesktopRunningForTest(func(context.Context) (bool, error) {
		return false, nil
	}))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	rt := New(t.Context())
	require.IsType(t, &http.Transport{}, rt)

	client := &http.Client{Transport: rt}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, http.NoBody)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestSanitizeURLForLog(t *testing.T) {
	t.Parallel()

	u, err := url.Parse("https://user:password@example.com/path?token=secret#fragment")
	require.NoError(t, err)

	// Scheme+host only: userinfo, path, query, fragment all stripped
	// to avoid leaking webhook credentials embedded in the path.
	assert.Equal(t, "https://example.com", sanitizeURLForLog(u))
	assert.Empty(t, sanitizeURLForLog(&url.URL{}))
}

func TestFallbackTransportSanitizesLogFields(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	proxy := &http.Transport{
		Proxy: func(*http.Request) (*url.URL, error) {
			return &url.URL{Scheme: "http", Host: "proxy.invalid"}, nil
		},
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("dial unix\nproxy socket: no such file or directory")
		},
	}
	fallback := newFallbackTransport(proxy, &http.Transport{})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/path?token=secret#fragment", http.NoBody)
	require.NoError(t, err)
	req.URL.User = url.UserPassword("user", "password")
	req.Body = nil

	resp, err := fallback.RoundTrip(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	log := logs.String()
	assert.Contains(t, log, "url=http://")
	assert.NotContains(t, log, "user:password")
	assert.NotContains(t, log, "token=secret")
	assert.NotContains(t, log, "fragment")
	assert.NotContains(t, log, "\\n")
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
			name:     "proxyconnect tcp with dial tcp error",
			errStr:   "proxyconnect tcp: dial tcp 10.0.0.1:443: connect: connection refused",
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
			name:     "target TCP connection refused",
			errStr:   "dial tcp 127.0.0.1:8080: connect: connection refused",
			expected: false,
		},
		{
			name:     "target HTTP request dial refusal",
			errStr:   "Get \"http://127.0.0.1:8080\": dial tcp 127.0.0.1:8080: connect: connection refused",
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

	// Disable compression. Only the proxy transport should be mutated here;
	// direct is owned by desktopAwareTransport and already configured before
	// this method is reached (see desktopAwareTransport.DisableCompression).
	ft.DisableCompression()

	// Verify compression is now disabled on the proxy transport.
	assert.True(t, proxy.DisableCompression)
	// direct must NOT be mutated here — mutating it would race with
	// in-flight requests on concurrent goroutines.
	assert.False(t, direct.DisableCompression)
}

// testError is a simple error type for testing
type testError struct {
	msg string
}

func (e *testError) Error() string {
	return e.msg
}

type countingRoundTripper struct {
	calls atomic.Int32
	err   error
}

func (c *countingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	c.calls.Add(1)
	if c.err != nil {
		return nil, c.err
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       http.NoBody,
		Request:    req,
	}, nil
}

// fakeFallback drives the fallbackTransport state machine against RoundTripper
// fakes so tests don't need a real Unix socket.
type fakeFallback struct {
	*fallbackTransport

	fakeProxy  *countingRoundTripper
	fakeDirect *countingRoundTripper
}

func (f *fakeFallback) RoundTrip(req *http.Request) (*http.Response, error) {
	if !f.proxyEnabled() {
		return f.fakeDirect.RoundTrip(req)
	}
	resp, err := f.fakeProxy.RoundTrip(req)
	if err == nil {
		return resp, nil
	}
	if isProxySocketError(err) {
		f.disableProxy()
		return f.fakeDirect.RoundTrip(req)
	}
	return nil, err
}

func newFakeFallback(proxyErr, directErr error) *fakeFallback {
	return &fakeFallback{
		fallbackTransport: newFallbackTransport(&http.Transport{}, &http.Transport{}),
		fakeProxy:         &countingRoundTripper{err: proxyErr},
		fakeDirect:        &countingRoundTripper{err: directErr},
	}
}

func TestFallbackTransport_ProxyRecoversAfterCooldown(t *testing.T) {
	t.Parallel()

	proxySocketErr := &testError{msg: "proxyconnect tcp: dial unix /path/to/httpproxy.sock: connect: no such file or directory"}
	ft := newFakeFallback(proxySocketErr, nil)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.invalid/", http.NoBody)
	require.NoError(t, err)

	// 1st request: proxy fails → direct.
	resp, err := ft.RoundTrip(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, int32(1), ft.fakeProxy.calls.Load())
	require.Equal(t, int32(1), ft.fakeDirect.calls.Load())

	// 2nd request: still on cooldown, proxy is skipped.
	resp, err = ft.RoundTrip(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, int32(1), ft.fakeProxy.calls.Load(), "proxy should be skipped during cooldown")
	require.Equal(t, int32(2), ft.fakeDirect.calls.Load())

	// Push the deadline into the past to exercise the CAS clear branch in
	// proxyEnabled (Store(0) would short-circuit it).
	ft.disabledUntilUnixNano.Store(time.Now().Add(-time.Second).UnixNano())
	ft.fakeProxy = &countingRoundTripper{}

	// 3rd request: cooldown elapsed, proxy re-probed and succeeds.
	resp, err = ft.RoundTrip(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, int32(1), ft.fakeProxy.calls.Load(), "healthy proxy should be tried again once cooldown expires")
	assert.Equal(t, int32(2), ft.fakeDirect.calls.Load(), "direct should not be hit once proxy recovers")
}

// Non-socket errors (e.g. upstream timeouts) must not trip the cooldown,
// otherwise a transient upstream problem would look like a proxy outage.
func TestFallbackTransport_NonSocketErrorDoesNotDisableProxy(t *testing.T) {
	t.Parallel()

	upstreamErr := &testError{msg: "read tcp 10.0.0.1:443: i/o timeout"}
	ft := newFakeFallback(upstreamErr, nil)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.invalid/", http.NoBody)
	require.NoError(t, err)

	resp, err := ft.RoundTrip(req) //nolint:bodyclose // resp is nil on error, checked below
	require.Error(t, err)
	require.Nil(t, resp)
	assert.True(t, errors.Is(err, upstreamErr) || err.Error() == upstreamErr.Error())
	assert.True(t, ft.proxyEnabled(), "proxy must stay enabled after a non-socket error")
	assert.Equal(t, int32(0), ft.fakeDirect.calls.Load(), "direct must not be tried on non-socket errors")
}

func TestFallbackTransport_ProxyEnabledAfterCooldownExpires(t *testing.T) {
	t.Parallel()

	ft := newFallbackTransport(&http.Transport{}, &http.Transport{})

	require.True(t, ft.proxyEnabled())

	ft.disabledUntilUnixNano.Store(time.Now().Add(-time.Second).UnixNano())
	require.True(t, ft.proxyEnabled(), "expired cooldown should re-enable the proxy")
	require.Equal(t, int64(0), ft.disabledUntilUnixNano.Load(), "expired cooldown should be cleared")

	future := time.Now().Add(time.Hour).UnixNano()
	ft.disabledUntilUnixNano.Store(future)
	require.False(t, ft.proxyEnabled())
	require.Equal(t, future, ft.disabledUntilUnixNano.Load())
}
