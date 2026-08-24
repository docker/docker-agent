package httpclient

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"syscall"
	"time"
)

// mustCIDR parses a CIDR string and panics if it is invalid. Intended for
// package-level var initialisation so bad literals fail at startup.
func mustCIDR(s string) *net.IPNet {
	_, network, err := net.ParseCIDR(s)
	if err != nil {
		panic("httpclient: invalid CIDR " + s + ": " + err.Error())
	}
	return network
}

// cgnatRange is RFC 6598 (100.64.0.0/10). Go's IsPrivate does not cover it.
var cgnatRange = mustCIDR("100.64.0.0/10")

// blockedIPv6Prefixes are IPv6 ranges that embed or forward arbitrary IPv4
// addresses and must therefore be treated as non-public.
var blockedIPv6Prefixes = []*net.IPNet{
	mustCIDR("2002::/16"),      // RFC 3056  – 6to4 (bits 16-47 = IPv4)
	mustCIDR("64:ff9b::/96"),   // RFC 6052  – NAT64 well-known
	mustCIDR("64:ff9b:1::/48"), // RFC 8215  – NAT64 local-use
	mustCIDR("fec0::/10"),      // RFC 3879  – site-local (deprecated)
}

// IsPublicIP reports whether ip is a routable public address. It rejects
// loopback (127/8, ::1), RFC1918 private ranges, link-local (incl. the
// 169.254.169.254 cloud metadata endpoint), multicast, the unspecified
// address (0.0.0.0, ::), CGNAT (100.64.0.0/10), IPv6 6to4, NAT64, and
// site-local prefixes.
func IsPublicIP(ip net.IP) bool {
	if ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified() {
		return false
	}
	if cgnatRange.Contains(ip) {
		return false
	}
	for _, prefix := range blockedIPv6Prefixes {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

// SSRFDialControl is invoked by net.Dialer after DNS resolution but before
// the TCP handshake. It rejects addresses that are not safe to fetch from
// over the public internet.
//
// Performing the check post-resolution defeats DNS rebinding: an attacker
// cannot point a public hostname at 127.0.0.1 or 169.254.169.254 to bypass
// us, because we re-validate the resolved IP itself.
func SSRFDialControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("parsing dial address %q: %w", address, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("refusing to dial %q: not a valid IP", host)
	}
	if !IsPublicIP(ip) {
		return fmt.Errorf("refusing to dial non-public address %s", ip)
	}
	return nil
}

// NewSSRFSafeTransport returns an [http.Transport] whose dialer enforces
// [SSRFDialControl] on every connection.
//
// When [http.DefaultTransport] is a plain *[http.Transport] (the default),
// the returned transport is a Clone() of it, inheriting proxy, TLS, idle-pool
// and HTTP/2 settings. When DefaultTransport has been replaced by a wrapper
// (e.g. an application that swapped in otelhttp.NewTransport for global
// observability) the clone is not possible; the function falls back to a
// minimal transport with proxy support and emits a warning via [log/slog].
// In either case the SSRF dial guard is active.
//
// Use this for outbound HTTP that may follow attacker-influenced URLs
// (OpenAPI specs whose servers[] list is taken from the spec body,
// user-configured API endpoints, etc.). It does not enforce HTTPS —
// callers that require it must validate the request URL themselves
// and/or supply a CheckRedirect on the surrounding *http.Client.
//
// As an exception, the explicitly-configured HTTP/HTTPS/ALL proxy is
// always dialable, even if it lives on a private address. Refusing to
// dial the operator-configured proxy adds no SSRF protection (the proxy
// enforces destination policy itself) and breaks sandboxes — like
// docker-agent's — whose mandatory egress proxy is on an RFC1918 IP.
func NewSSRFSafeTransport() *http.Transport {
	var t *http.Transport
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		t = base.Clone()
		t.Proxy = environmentProxyFunc()
	} else {
		// http.DefaultTransport has been replaced by a wrapper (e.g. otelhttp).
		// We can't clone settings we can't see, so fall back to a minimal
		// transport. Callers that need the full DefaultTransport configuration
		// should call NewSSRFSafeTransport before any global replacement.
		slog.Warn("httpclient: http.DefaultTransport is not a *http.Transport; "+
			"NewSSRFSafeTransport is using a minimal fallback transport — "+
			"proxy env vars are honoured but other DefaultTransport settings are not inherited",
			"type", fmt.Sprintf("%T", http.DefaultTransport))
		t = &http.Transport{Proxy: environmentProxyFunc()}
	}
	proxies := proxyDialAllowlist()
	guarded := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   SSRFDialControl,
	}
	trusted := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	// The address here is pre-resolution: with a proxy configured the
	// transport dials the proxy's literal host:port, so the allowlist can
	// be matched by string without ever resolving the proxy hostname.
	t.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if _, ok := proxies[canonicalHostPort(address)]; ok {
			return trusted.DialContext(ctx, network, address)
		}
		return guarded.DialContext(ctx, network, address)
	}
	return t
}

// canonicalHostPort normalises a "host:port" dial address so equivalent
// IPv4 forms compare equal. An IPv4-mapped IPv6 address (::ffff:a.b.c.d)
// is folded back to its dotted-quad form, matching what proxyHostPort
// stores in the allowlist for an IPv4-literal proxy. Non-IP hosts and
// malformed inputs are returned unchanged so the allowlist lookup
// simply misses and the SSRF fall-through applies.
func canonicalHostPort(address string) string {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return address
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return address
	}
	if ip4 := ip.To4(); ip4 != nil {
		return net.JoinHostPort(ip4.String(), port)
	}
	return net.JoinHostPort(ip.String(), port)
}

// proxyDialAllowlist returns the set of "host:port" strings the
// HTTP_PROXY / HTTPS_PROXY / ALL_PROXY environment variables name.
// Hostnames are kept verbatim — never resolved — because the transport
// dials a configured proxy by its literal host:port, so the comparison
// happens pre-resolution. Constructors must not block on DNS.
// The result is a snapshot taken at call time — safe to capture in a
// dial closure.
func proxyDialAllowlist() map[string]struct{} {
	out := map[string]struct{}{}
	raw := []string{
		os.Getenv("HTTP_PROXY"), os.Getenv("http_proxy"),
		os.Getenv("HTTPS_PROXY"), os.Getenv("https_proxy"),
		os.Getenv("ALL_PROXY"), os.Getenv("all_proxy"),
	}
	for _, s := range raw {
		if addr := proxyHostPort(s); addr != "" {
			out[addr] = struct{}{}
		}
	}
	return out
}

// proxyHostPort parses a single proxy specification (matching the syntax
// accepted by net/http.ProxyFromEnvironment: a full URL or a bare host[:port]
// in which case http:// is implied) and returns its canonical "host:port".
// An empty or unparseable input yields "".
func proxyHostPort(spec string) string {
	if spec == "" {
		return ""
	}
	u, err := url.Parse(spec)
	if err != nil || u.Host == "" {
		u, err = url.Parse("http://" + spec)
		if err != nil || u.Host == "" {
			return ""
		}
	}
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "https":
			port = "443"
		case "socks5", "socks5h":
			port = "1080"
		default:
			port = "80"
		}
	}
	return canonicalHostPort(net.JoinHostPort(u.Hostname(), port))
}

// BoundedRedirects returns an http.Client.CheckRedirect that limits a
// redirect chain to maxHops. SSRF on each redirect target is enforced
// by the transport's dialer; this only prevents infinite loops.
func BoundedRedirects(maxHops int) func(*http.Request, []*http.Request) error {
	return func(_ *http.Request, via []*http.Request) error {
		if len(via) >= maxHops {
			return fmt.Errorf("stopped after %d redirects", maxHops)
		}
		return nil
	}
}

// HTTPSOnlyRedirects returns an http.Client.CheckRedirect that limits the
// redirect chain to maxHops AND rejects redirects whose Location is not
// https://. Use this when the original request is required to be HTTPS
// and a TLS downgrade through a Location header must be prevented.
func HTTPSOnlyRedirects(maxHops int) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxHops {
			return fmt.Errorf("stopped after %d redirects", maxHops)
		}
		if req.URL.Scheme != "https" {
			return fmt.Errorf("refusing redirect to non-https URL %q", req.URL.Redacted())
		}
		return nil
	}
}

// LocalhostOnlyRedirects returns an http.Client.CheckRedirect that limits
// the redirect chain to maxHops AND requires every redirect target to be
// an http://localhost URL. This prevents a localhost service from redirecting
// to internal network addresses (e.g. cloud metadata endpoints), which
// would bypass SSRF protection.
func LocalhostOnlyRedirects(maxHops int) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxHops {
			return fmt.Errorf("stopped after %d redirects", maxHops)
		}
		if req.URL.Scheme != "http" || req.URL.Hostname() != "localhost" {
			return fmt.Errorf("refusing redirect to non-localhost URL %q", req.URL.Redacted())
		}
		return nil
	}
}
