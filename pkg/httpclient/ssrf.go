package httpclient

import (
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"
)

// IsPublicIP reports whether ip is a routable public address. It rejects
// loopback (127/8, ::1), RFC1918 private ranges, link-local (incl. the
// 169.254.169.254 cloud metadata endpoint), multicast and the unspecified
// address (0.0.0.0, ::).
func IsPublicIP(ip net.IP) bool {
	return !ip.IsLoopback() &&
		!ip.IsPrivate() &&
		!ip.IsLinkLocalUnicast() &&
		!ip.IsLinkLocalMulticast() &&
		!ip.IsMulticast() &&
		!ip.IsUnspecified()
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

// NewSSRFSafeTransport returns a clone of [http.DefaultTransport] whose
// dialer enforces [SSRFDialControl] on every connection. All other settings
// — proxy, idle pool, HTTP/2, timeouts — are inherited so the transport
// keeps up with future stdlib changes.
//
// Use this for outbound HTTP that may follow attacker-influenced URLs
// (OpenAPI specs whose servers[] list is taken from the spec body,
// user-configured API endpoints, etc.). It does not enforce HTTPS —
// callers that require it must validate the request URL themselves
// and/or supply a CheckRedirect on the surrounding *http.Client.
func NewSSRFSafeTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   SSRFDialControl,
	}).DialContext
	return t
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
