package httpclient

import (
	"net/http"
	"time"
)

// DefaultToolHTTPTimeout is the HTTP client timeout used by the built-in
// HTTP-based toolsets (`fetch`, `api`, `openapi`, `a2a`) when the operator
// does not override it via `timeout:` in the agent config.
//
// Centralised so the four toolsets agree on a single default — changing
// this value uniformly affects every HTTP-based built-in tool.
const DefaultToolHTTPTimeout = 30 * time.Second

var (
	safeTransport            = NewDesktopAwareSSRFSafeTransport()
	allowPrivateIPsTransport = newAllowPrivateIPsTransport()
)

// TransportForAllowPrivateIPs returns the shared transport for the requested
// outbound address policy.
func TransportForAllowPrivateIPs(allowPrivateIPs bool) http.RoundTripper {
	if allowPrivateIPs {
		return allowPrivateIPsTransport
	}
	return safeTransport
}

// ClientForAllowPrivateIPs returns a client with an independent timeout and
// shared transport for the requested outbound address policy.
func ClientForAllowPrivateIPs(timeout time.Duration, allowPrivateIPs bool) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		Transport:     TransportForAllowPrivateIPs(allowPrivateIPs),
		CheckRedirect: BoundedRedirects(10),
	}
}

// NewAllowPrivateIPsClient returns an HTTP client for explicit
// allow_private_ips opt-ins. It can reach private addresses directly, but uses
// Docker Desktop's PAC proxy when available; loopback is always direct. Docker
// Desktop remains optional and DOCKER_AGENT_DISABLE_DESKTOP_PROXY=1 restores the
// default environment-proxy/direct behavior.
func NewAllowPrivateIPsClient(timeout time.Duration) *http.Client {
	return ClientForAllowPrivateIPs(timeout, true)
}

// NewSafeClient returns the HTTP client used by built-in tools that issue
// outbound calls to URLs the operator (or a fetched OpenAPI spec) supplies.
//
// On its direct path — Docker Desktop unavailable, disabled via
// DOCKER_AGENT_DISABLE_DESKTOP_PROXY, or the target host is outside the
// Docker-owned allowlist (docker.com and docker.io families) — the client
// refuses non-public IPs at dial time, defeating DNS rebinding to loopback /
// RFC1918 / link-local incl. cloud metadata at 169.254.169.254.
//
// When Docker Desktop is running, only Docker-owned hostnames (docker.com,
// docker.io and their subdomains) go through its PAC proxy. There the only
// local control is a pre-request DNS preflight (see proxySafe): Desktop
// selects and resolves the destination itself, so dial-time enforcement does
// not apply. Either way the redirect chain is bounded at 10 hops.
//
// When unsafe is true the client uses [http.DefaultTransport]. This branch
// exists ONLY for tests, which use [httptest.NewServer] (binds to 127.0.0.1)
// and therefore cannot pass the SSRF check.
func NewSafeClient(timeout time.Duration, unsafe bool) *http.Client {
	if unsafe {
		return &http.Client{Timeout: timeout}
	}
	return ClientForAllowPrivateIPs(timeout, false)
}
