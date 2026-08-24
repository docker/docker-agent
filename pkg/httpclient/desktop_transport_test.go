package httpclient

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/http/httpproxy"

	desktoptransport "github.com/docker/docker-agent/pkg/desktop/transport"
)

// legacyDisableDesktopProxyEnv is the retired name that must remain inert.
const legacyDisableDesktopProxyEnv = "CAGENT_DISABLE_DESKTOP_PROXY"

func TestDesktopAwareTransportProxySafe(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		host     string
		resolver func(context.Context, string) ([]net.IP, error)
		want     bool
	}{
		{
			name: "literal private address",
			host: "127.0.0.1",
			want: false,
		},
		{
			name: "all resolved addresses private",
			host: "internal.example",
			resolver: func(context.Context, string) ([]net.IP, error) {
				return []net.IP{net.ParseIP("10.0.0.1"), net.ParseIP("192.168.1.1")}, nil
			},
			want: false,
		},
		{
			name: "mixed public and private addresses stay direct",
			host: "mixed.example",
			resolver: func(context.Context, string) ([]net.IP, error) {
				return []net.IP{net.ParseIP("10.0.0.1"), net.ParseIP("1.1.1.1")}, nil
			},
			want: false,
		},
		{
			name: "all resolved addresses public",
			host: "public.example",
			resolver: func(context.Context, string) ([]net.IP, error) {
				return []net.IP{net.ParseIP("1.1.1.1"), net.ParseIP("2606:4700:4700::1111")}, nil
			},
			want: true,
		},
		{
			name: "empty DNS response stays direct",
			host: "empty.example",
			resolver: func(context.Context, string) ([]net.IP, error) {
				return nil, nil
			},
			want: false,
		},
		{
			name: "temporary DNS error stays direct",
			host: "temporary.example",
			resolver: func(context.Context, string) ([]net.IP, error) {
				return nil, &net.DNSError{IsTemporary: true}
			},
			want: false,
		},
		{
			name: "non-DNS resolver error stays direct",
			host: "failed.example",
			resolver: func(context.Context, string) ([]net.IP, error) {
				return nil, errors.New("resolver failed")
			},
			want: false,
		},
		{
			name: "NXDOMAIN fails closed (Option A: broken resolver, not PAC-only network)",
			host: "proxy-only.example",
			resolver: func(context.Context, string) ([]net.IP, error) {
				return nil, &net.DNSError{IsNotFound: true}
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			transport := newDesktopAwareTransport(true).(*desktopAwareTransport)
			if tt.resolver != nil {
				transport.resolver = tt.resolver
			}
			assert.Equal(t, tt.want, transport.proxySafe(t.Context(), tt.host))
		})
	}
}

func TestDesktopAwareTransportLoopbackIsNeverProxied(t *testing.T) {
	t.Parallel()

	assert.True(t, isLoopbackHost("localhost"))
	assert.True(t, isLoopbackHost("localhost."))
	assert.True(t, isLoopbackHost("LOCALHOST"))
	assert.True(t, isLoopbackHost("service.localhost"))
	assert.True(t, isLoopbackHost("127.0.0.1"))
	assert.True(t, isLoopbackHost("::1"))
	assert.False(t, isLoopbackHost("example.com"))
}

func TestDesktopAwareTransportProxyFunc(t *testing.T) {
	proxy := proxyFunc(&httpproxy.Config{
		HTTPSProxy: "http://proxy.example:8443",
		NoProxy:    "bypass.example,.internal.example",
	})

	for _, test := range []struct {
		host      string
		wantProxy bool
	}{
		{host: "public.example", wantProxy: true},
		{host: "bypass.example", wantProxy: false},
		{host: "service.internal.example", wantProxy: false},
	} {
		t.Run(test.host, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+test.host, http.NoBody)
			selected, err := proxy(req)
			require.NoError(t, err)
			assert.Equal(t, test.wantProxy, selected != nil)
		})
	}
}

func TestDesktopAwareTransportDesktopWinsOverNoProxyUntilKillSwitch(t *testing.T) {
	// Uses hub.docker.com (a Docker host in the allowlist) so the guarded
	// transport routes through Desktop regardless of NO_PROXY settings.
	t.Setenv(disableDesktopProxyEnv, "")
	t.Cleanup(desktoptransport.SetDesktopRunningForTest(func(context.Context) (bool, error) { return true, nil }))

	transport := newDesktopAwareTransport(true).(*desktopAwareTransport)
	transport.resolver = func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("52.0.0.1")}, nil }
	desktop := &countingTransport{}
	var proxySelections, directDials int
	transport.direct.Proxy = func(req *http.Request) (*url.URL, error) {
		proxySelections++
		return proxyFunc(&httpproxy.Config{HTTPProxy: "http://proxy.example:8080", NoProxy: "hub.docker.com"})(req)
	}
	transport.direct.DialContext = func(context.Context, string, string) (net.Conn, error) {
		directDials++
		return nil, errors.New("direct dial")
	}
	transport.newDesktopTransport = func(context.Context, http.RoundTripper) http.RoundTripper { return desktop }

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://hub.docker.com", http.NoBody)
	require.NoError(t, err)

	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, 1, desktop.calls)
	assert.Zero(t, proxySelections)
	assert.Zero(t, directDials)

	t.Setenv(disableDesktopProxyEnv, "1")
	resp, err = transport.RoundTrip(req)
	if resp != nil {
		require.NoError(t, resp.Body.Close())
	}
	require.EqualError(t, err, "direct dial")
	assert.Equal(t, 1, desktop.calls)
	assert.Equal(t, 1, proxySelections, "the direct path must apply NO_PROXY")
	assert.Equal(t, 1, directDials, "NO_PROXY must select a direct connection")
}

func TestDesktopAwareTransportCachesDesktopTransport(t *testing.T) {
	t.Cleanup(desktoptransport.SetDesktopRunningForTest(func(context.Context) (bool, error) { return true, nil }))

	transport := newDesktopAwareTransport(false).(*desktopAwareTransport)
	var factoryCalls int
	branch := &countingTransport{}
	transport.newDesktopTransport = func(context.Context, http.RoundTripper) http.RoundTripper {
		factoryCalls++
		return branch
	}

	for range 2 {
		resp := roundTrip(t, transport)
		require.NoError(t, resp.Body.Close())
	}
	assert.Equal(t, 1, factoryCalls)
	assert.Equal(t, 2, branch.calls)
}

func TestDesktopAwareTransportDisabledOrGuardedUsesDirect(t *testing.T) {
	for _, tc := range []struct {
		name     string
		guarded  bool
		host     string
		disabled bool
	}{
		{name: "kill switch", host: "example.com", disabled: true},
		{name: "guarded loopback", guarded: true, host: "127.0.0.1"},
		{name: "unguarded loopback", host: "127.0.0.1"},
		{name: "guarded non-Docker goes direct (allowlist)", guarded: true, host: "example.com"},
		{name: "guarded non-Docker arbitrary subdomain goes direct", guarded: true, host: "evil.docker.com.attacker.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.disabled {
				t.Setenv(disableDesktopProxyEnv, "1")
			}
			transport := newDesktopAwareTransport(tc.guarded).(*desktopAwareTransport)
			transport.resolver = func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("10.0.0.1")}, nil }
			transport.newDesktopTransport = func(context.Context, http.RoundTripper) http.RoundTripper {
				t.Fatal("Desktop transport must not be used")
				return nil
			}
			transport.direct = &http.Transport{}
			transport.direct.RegisterProtocol("https", roundTripperFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
			}))
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+tc.host, http.NoBody)
			require.NoError(t, err)
			resp, err := transport.RoundTrip(req)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
		})
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestDesktopAwareTransportConsultsDesktopDetectionPerRequest(t *testing.T) {
	desktopRunning := false
	var detections int
	t.Cleanup(desktoptransport.SetDesktopRunningForTest(func(context.Context) (bool, error) {
		detections++
		return desktopRunning, nil
	}))

	transport := newDesktopAwareTransport(false).(*desktopAwareTransport)
	direct := &countingTransport{}
	transport.direct = &http.Transport{}
	transport.direct.RegisterProtocol("https", direct)
	var factoryCalls int
	branch := &countingTransport{}
	transport.newDesktopTransport = func(context.Context, http.RoundTripper) http.RoundTripper {
		factoryCalls++
		return branch
	}

	for range 2 {
		resp := roundTrip(t, transport)
		require.NoError(t, resp.Body.Close())
	}
	assert.Equal(t, 2, detections)
	assert.Equal(t, 2, direct.calls)
	assert.Zero(t, factoryCalls)

	desktopRunning = true
	for range 2 {
		resp := roundTrip(t, transport)
		require.NoError(t, resp.Body.Close())
	}
	assert.Equal(t, 4, detections)
	assert.Equal(t, 1, factoryCalls)
	assert.Equal(t, 2, branch.calls)

	desktopRunning = false
	resp := roundTrip(t, transport)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, 5, detections)
	assert.Equal(t, 1, factoryCalls)
	assert.Equal(t, 3, direct.calls)
}

func TestDesktopAwareTransportDisableCompressionBeforeDesktopTransport(t *testing.T) {
	t.Cleanup(desktoptransport.SetDesktopRunningForTest(func(context.Context) (bool, error) { return true, nil }))

	transport := newDesktopAwareTransport(false).(*desktopAwareTransport)
	transport.direct = &http.Transport{}
	transport.direct.RegisterProtocol("https", roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
	}))
	transport.DisableCompression()
	proxy := &compressionTransport{}
	transport.newDesktopTransport = func(context.Context, http.RoundTripper) http.RoundTripper { return proxy }

	resp := roundTrip(t, transport)
	require.NoError(t, resp.Body.Close())
	assert.True(t, proxy.disabled)
}

func TestDesktopAwareTransportDisableCompressionConcurrentDesktopCreation(t *testing.T) {
	t.Cleanup(desktoptransport.SetDesktopRunningForTest(func(context.Context) (bool, error) { return true, nil }))

	transport := newDesktopAwareTransport(false).(*desktopAwareTransport)
	proxy := &compressionTransport{}
	transport.newDesktopTransport = func(context.Context, http.RoundTripper) http.RoundTripper { return proxy }

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			transport.DisableCompression()
		}
	}()
	for range 100 {
		resp := roundTrip(t, transport)
		require.NoError(t, resp.Body.Close())
	}
	<-done
	assert.True(t, proxy.disabled)
}

func TestDesktopAwareTransportDisableCompressionAcrossDirectAndDesktopFlaps(t *testing.T) {
	desktopRunning := false
	t.Cleanup(desktoptransport.SetDesktopRunningForTest(func(context.Context) (bool, error) {
		return desktopRunning, nil
	}))

	transport := newDesktopAwareTransport(false).(*desktopAwareTransport)
	direct := &countingTransport{}
	transport.direct = &http.Transport{}
	transport.direct.RegisterProtocol("https", direct)
	desktop := &compressionTransport{}
	var factoryCalls int
	transport.newDesktopTransport = func(context.Context, http.RoundTripper) http.RoundTripper {
		factoryCalls++
		return desktop
	}

	transport.DisableCompression()
	assert.True(t, transport.direct.DisableCompression)

	resp := roundTrip(t, transport)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, 1, direct.calls)
	assert.Zero(t, desktop.calls)

	desktopRunning = true
	resp = roundTrip(t, transport)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, 1, factoryCalls)
	assert.Equal(t, 1, desktop.calls)
	assert.True(t, desktop.disabled)

	desktopRunning = false
	resp = roundTrip(t, transport)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, 2, direct.calls)

	desktopRunning = true
	resp = roundTrip(t, transport)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, 1, factoryCalls)
	assert.Equal(t, 2, desktop.calls)
	assert.True(t, desktop.disabled)
}

func TestDesktopAwareTransportRetainsDesktopBranchCooldown(t *testing.T) {
	t.Cleanup(desktoptransport.SetDesktopRunningForTest(func(context.Context) (bool, error) { return true, nil }))

	transport := newDesktopAwareTransport(false).(*desktopAwareTransport)
	branch := &cooldownTransport{}
	var factoryCalls int
	transport.newDesktopTransport = func(context.Context, http.RoundTripper) http.RoundTripper {
		factoryCalls++
		return branch
	}

	for range 2 {
		resp := roundTrip(t, transport)
		require.NoError(t, resp.Body.Close())
	}
	assert.Equal(t, 1, factoryCalls)
	assert.Equal(t, 1, branch.proxyCalls)
	assert.Equal(t, 2, branch.directCalls)
}

func roundTrip(t *testing.T, transport http.RoundTripper) *http.Response {
	t.Helper()
	req := requestForHost(t, "example.com")
	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	return resp
}

func requestForHost(t *testing.T, host string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+host, http.NoBody)
	require.NoError(t, err)
	return req
}

type countingTransport struct {
	calls int
}

func (t *countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls++
	return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
}

type cooldownTransport struct {
	proxyCalls  int
	directCalls int
	disabled    bool
}

func (t *cooldownTransport) RoundTrip(*http.Request) (*http.Response, error) {
	if !t.disabled {
		t.proxyCalls++
		t.disabled = true
	}
	t.directCalls++
	return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
}

type compressionTransport struct {
	disabled bool
	calls    int
}

func (t *compressionTransport) DisableCompression() {
	t.disabled = true
}

func (t *compressionTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls++
	return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
}

func TestIsDockerHost(t *testing.T) {
	t.Parallel()
	tests := []struct {
		host     string
		expected bool
	}{
		{"docker.com", true},
		{"docker.io", true},
		{"DOCKER.COM", true},
		{"hub.docker.com", true},
		{"registry-1.docker.io", true},
		{"auth.docker.io", true},
		{"index.docker.io", true},
		{"cdn.registry.docker.io", true},
		{"desktop.docker.com", true},
		{"api.docker.com", true},
		{"docker.com.", true},
		{"hub.docker.com.", true},
		{"example.com", false},
		{"jenkins.internal", false},
		{"evil.localhost", false},
		{"notdocker.com", false},
		{"docker.com.attacker.com", false},
		{"evil.docker.com.attacker.com", false},
		{"fakedocker.com", false},
		{"xdocker.com", false},
		{"docker.org", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, isDockerHost(tt.host))
		})
	}
}

func TestDesktopAwareTransportUnguardedUsesDesktopForNonDockerHost(t *testing.T) {
	desktopHit := false
	transport := newDesktopAwareTransport(false).(*desktopAwareTransport)
	transport.resolver = func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("1.1.1.1")}, nil
	}
	transport.newDesktopTransport = func(_ context.Context, _ http.RoundTripper) http.RoundTripper {
		return roundTripperFunc(func(_ *http.Request) (*http.Response, error) {
			desktopHit = true
			return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
		})
	}
	t.Cleanup(desktoptransport.SetDesktopRunningForTest(func(context.Context) (bool, error) { return true, nil }))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com", http.NoBody)
	require.NoError(t, err)
	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.True(t, desktopHit, "unguarded transport should route non-Docker host through Desktop")
}

func TestDesktopAwareTransportGuardedDockerHostUsesDesktop(t *testing.T) {
	desktopHit := false
	transport := newDesktopAwareTransport(true).(*desktopAwareTransport)
	transport.resolver = func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("52.0.0.1")}, nil
	}
	transport.newDesktopTransport = func(_ context.Context, _ http.RoundTripper) http.RoundTripper {
		return roundTripperFunc(func(_ *http.Request) (*http.Response, error) {
			desktopHit = true
			return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
		})
	}
	t.Cleanup(desktoptransport.SetDesktopRunningForTest(func(context.Context) (bool, error) { return true, nil }))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://hub.docker.com/v2/", http.NoBody)
	require.NoError(t, err)
	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.True(t, desktopHit, "guarded transport should route Docker host through Desktop when it resolves to a public IP")
}

func TestDesktopAwareTransportGuardedDockerHostNXDOMAINStaysDirect(t *testing.T) {
	desktopRoundTrips := 0
	directRoundTrips := 0
	transport := newDesktopAwareTransport(true).(*desktopAwareTransport)
	transport.resolver = func(context.Context, string) ([]net.IP, error) {
		return nil, &net.DNSError{IsNotFound: true}
	}
	transport.newDesktopTransport = func(_ context.Context, _ http.RoundTripper) http.RoundTripper {
		return roundTripperFunc(func(_ *http.Request) (*http.Response, error) {
			desktopRoundTrips++
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
		})
	}
	transport.direct = &http.Transport{}
	transport.direct.RegisterProtocol("https", roundTripperFunc(func(_ *http.Request) (*http.Response, error) {
		directRoundTrips++
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
	}))
	t.Cleanup(desktoptransport.SetDesktopRunningForTest(func(context.Context) (bool, error) { return true, nil }))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://hub.docker.com/v2/", http.NoBody)
	require.NoError(t, err)
	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Zero(t, desktopRoundTrips, "Desktop RoundTrip must not be called when proxySafe returns false (NXDOMAIN)")
	assert.Equal(t, 1, directRoundTrips, "direct transport must handle the request when proxySafe returns false")
}

func TestDesktopAwareTransportGuardedDockerHostPrivateIPStaysDirect(t *testing.T) {
	desktopRoundTrips := 0
	directRoundTrips := 0
	transport := newDesktopAwareTransport(true).(*desktopAwareTransport)
	transport.resolver = func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("10.0.0.1")}, nil
	}
	transport.newDesktopTransport = func(_ context.Context, _ http.RoundTripper) http.RoundTripper {
		return roundTripperFunc(func(_ *http.Request) (*http.Response, error) {
			desktopRoundTrips++
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
		})
	}
	transport.direct = &http.Transport{}
	transport.direct.RegisterProtocol("https", roundTripperFunc(func(_ *http.Request) (*http.Response, error) {
		directRoundTrips++
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
	}))
	t.Cleanup(desktoptransport.SetDesktopRunningForTest(func(context.Context) (bool, error) { return true, nil }))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://hub.docker.com/v2/", http.NoBody)
	require.NoError(t, err)
	resp, err := transport.RoundTrip(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Zero(t, desktopRoundTrips, "Desktop RoundTrip must not be called when proxySafe returns false (private IP)")
	assert.Equal(t, 1, directRoundTrips, "direct transport must handle the request when proxySafe returns false")
}

func TestDesktopProxyDisabled(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"", false},
		{"0", false},
		{"false", false},
		{"anything", false},
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{" yes ", true},
		{"On", true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv(disableDesktopProxyEnv, tc.value)
			t.Setenv(legacyDisableDesktopProxyEnv, "")
			assert.Equal(t, tc.want, desktopProxyDisabled())
		})
	}

	t.Setenv(disableDesktopProxyEnv, "")
	t.Setenv(legacyDisableDesktopProxyEnv, "1")
	assert.False(t, desktopProxyDisabled())
}

func TestDesktopProxyDisabledWarnsOncePerInvalidValue(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	resetInvalidDesktopProxySetting()

	t.Setenv(disableDesktopProxyEnv, "unexpected")
	assert.False(t, desktopProxyDisabled())
	assert.False(t, desktopProxyDisabled())
	assert.Equal(t, 1, strings.Count(logs.String(), "unrecognized DOCKER_AGENT_DISABLE_DESKTOP_PROXY value"))

	t.Setenv(disableDesktopProxyEnv, "other")
	assert.False(t, desktopProxyDisabled())
	assert.Equal(t, 2, strings.Count(logs.String(), "unrecognized DOCKER_AGENT_DISABLE_DESKTOP_PROXY value"))

	t.Setenv(disableDesktopProxyEnv, "")
	assert.False(t, desktopProxyDisabled())
	t.Setenv(disableDesktopProxyEnv, "unexpected")
	assert.False(t, desktopProxyDisabled())
	assert.Equal(t, 3, strings.Count(logs.String(), "unrecognized DOCKER_AGENT_DISABLE_DESKTOP_PROXY value"))
}
