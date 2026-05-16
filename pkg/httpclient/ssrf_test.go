package httpclient

import (
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsPublicIP(t *testing.T) {
	t.Parallel()

	tests := []struct {
		ip       string
		isPublic bool
	}{
		{"8.8.8.8", true},
		{"1.1.1.1", true},
		{"2001:4860:4860::8888", true}, // public IPv6

		{"127.0.0.1", false},
		{"::1", false},
		{"10.0.0.1", false},
		{"172.16.0.1", false},
		{"192.168.0.1", false},
		{"169.254.169.254", false}, // AWS/GCP/Azure metadata
		{"fe80::1", false},         // link-local IPv6
		{"224.0.0.1", false},       // multicast
		{"0.0.0.0", false},
		{"::", false},
	}
	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			t.Parallel()
			ip := net.ParseIP(tt.ip)
			require.NotNil(t, ip, "failed to parse IP %q", tt.ip)
			assert.Equal(t, tt.isPublic, IsPublicIP(ip))
		})
	}
}

func TestSSRFDialControl(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		address string
		wantErr string // empty means no error
	}{
		{"public IPv4", "8.8.8.8:443", ""},
		{"public IPv6", "[2001:4860:4860::8888]:443", ""},
		{"loopback IPv4", "127.0.0.1:80", "non-public"},
		{"loopback IPv6", "[::1]:80", "non-public"},
		{"RFC1918", "10.0.0.1:80", "non-public"},
		{"link-local cloud metadata", "169.254.169.254:80", "non-public"},
		{"unspecified", "0.0.0.0:80", "non-public"},
		{"hostname (not an IP)", "example.com:80", "not a valid IP"},
		{"missing port", "8.8.8.8", "parsing dial address"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := SSRFDialControl("tcp", tt.address, nil)
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestBoundedRedirects(t *testing.T) {
	t.Parallel()

	check := BoundedRedirects(3)

	mustParse := func(s string) *url.URL {
		u, err := url.Parse(s)
		require.NoError(t, err)
		return u
	}

	// Within bound: no error.
	for n := range 3 {
		t.Run("within-bound", func(t *testing.T) {
			via := make([]*http.Request, n)
			req := &http.Request{URL: mustParse("https://example.com/")}
			assert.NoError(t, check(req, via))
		})
	}

	// At the bound: rejected.
	via := make([]*http.Request, 3)
	req := &http.Request{URL: mustParse("https://example.com/")}
	err := check(req, via)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stopped after 3 redirects")
}

func TestHTTPSOnlyRedirects(t *testing.T) {
	t.Parallel()

	mustParse := func(s string) *url.URL {
		u, err := url.Parse(s)
		require.NoError(t, err)
		return u
	}

	tests := []struct {
		name    string
		target  string
		via     int    // length of the via slice
		wantErr string // empty means no error
	}{
		{"https redirect allowed", "https://example.com/agent.yaml", 1, ""},
		{"http redirect rejected", "http://example.com/agent.yaml", 1, "non-https"},
		{"file redirect rejected", "file:///etc/passwd", 1, "non-https"},
		{"javascript redirect rejected", "javascript:alert(1)", 1, "non-https"},
		{"ftp redirect rejected", "ftp://example.com/x", 1, "non-https"},
		{"redirect loop bounded", "https://example.com/agent.yaml", 10, "10 redirects"},
	}
	check := HTTPSOnlyRedirects(10)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := &http.Request{URL: mustParse(tt.target)}
			via := make([]*http.Request, tt.via)
			err := check(req, via)
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestNewSSRFSafeTransport_RefusesPrivateIP(t *testing.T) {
	t.Parallel()

	// Drive the SSRF check end-to-end through an http.Client. We don't
	// need a server: the dial-time hook fires before any TCP handshake.
	client := &http.Client{Transport: NewSSRFSafeTransport()}

	tests := []string{
		"http://127.0.0.1/",
		"http://[::1]/",
		"http://10.0.0.1/",
		"http://169.254.169.254/latest/meta-data/",
		"http://0.0.0.0/",
	}
	for _, target := range tests {
		t.Run(target, func(t *testing.T) {
			t.Parallel()
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, http.NoBody)
			require.NoError(t, err)
			resp, err := client.Do(req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			require.Error(t, err)
			assert.True(
				t,
				strings.Contains(err.Error(), "non-public address") ||
					strings.Contains(err.Error(), "not a valid IP"),
				"unexpected error %q", err.Error(),
			)
		})
	}
}

// TestIsPublicIP_IPv4MappedIPv6 is a regression test that pins Go's
// behaviour: IPv4-mapped IPv6 addresses (::ffff:a.b.c.d) inherit the
// classification of their embedded IPv4 address. This prevents an
// attacker from bypassing SSRF checks by wrapping 169.254.169.254
// in IPv6 notation.
func TestIsPublicIP_IPv4MappedIPv6(t *testing.T) {
	t.Parallel()

	tests := []struct {
		addr     string
		isPublic bool
	}{
		{"::ffff:127.0.0.1", false},       // loopback
		{"::ffff:10.0.0.1", false},        // RFC1918
		{"::ffff:169.254.169.254", false}, // cloud metadata (link-local)
		{"::ffff:8.8.8.8", true},          // public
	}

	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			t.Parallel()
			ip := net.ParseIP(tt.addr)
			require.NotNil(t, ip, "ParseIP must succeed")
			assert.Equal(t, tt.isPublic, IsPublicIP(ip))
		})
	}
}

// TestSSRFDialControl_IPv6ZoneID verifies that IPv6 addresses with zone
// identifiers (fe80::1%eth0) are rejected. net.ParseIP returns nil for
// such addresses, causing the dial control to fail-closed with "not a
// valid IP" rather than potentially bypassing the public-IP check.
func TestSSRFDialControl_IPv6ZoneID(t *testing.T) {
	t.Parallel()

	err := SSRFDialControl("tcp", "[fe80::1%eth0]:80", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a valid IP")
}
