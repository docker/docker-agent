package httpclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// EgressProxyURLParam is the query parameter under which the trusted egress
// proxy receives the original absolute target URL.
const EgressProxyURLParam = "url"

// EgressProxyLocationHeader carries the target's Location header back from
// the trusted egress proxy on redirect responses, in place of a real Location
// the browser would act on. See [WithEgressProxy].
const EgressProxyLocationHeader = "X-Docker-Agent-Location"

// ErrBrowserEgressUnsupported is returned by guarded transports on browser
// (js/wasm) builds when no egress proxy is configured on the request context.
// The browser's fetch API gives us no dial hook, so the SSRF guard cannot be
// enforced locally and the request fails closed.
var ErrBrowserEgressUnsupported = errors.New("httpclient: SSRF-guarded HTTP is not supported in the browser: " +
	"configure a trusted HTTPS egress proxy (toolProxy) or opt in with allow_private_ips: true")

type egressProxyKey struct{}

// WithEgressProxy returns a context that routes SSRF-guarded requests through
// the trusted HTTPS egress proxy at proxyURL. Only browser (js/wasm) guarded
// transports consult it; native transports keep dial-time enforcement and
// ignore the value.
//
// Protocol: the proxy receives the original absolute target URL in the
// [EgressProxyURLParam] query parameter, with the same method, body and
// headers as the original request. The proxy is trusted to resolve the
// target and enforce the destination IP policy. It must not follow the
// target's redirects: it relays a 301/302/303/307/308 with the same status,
// no Location header, and the target's Location (absolute, or relative to
// the target) in [EgressProxyLocationHeader], which it must list in
// Access-Control-Expose-Headers. The transport restores it as Location so
// the caller's http.Client.CheckRedirect sees real target URLs, strips
// credentials across hosts and bounds the chain exactly as for a direct
// request; each hop is a fresh request to the proxy. A redirect response
// that still carries a real Location is rejected.
//
// Request headers — including Authorization and other credentials meant for
// the target — are forwarded to the proxy intentionally, so proxyURL must
// point at infrastructure the operator controls. The fetch runs with
// credentials "omit", so the page's cookies never ride along; the proxy must
// still strip what the browser adds on its own — Cookie, Origin, Referer and
// Host — before forwarding, so the target sees only the request Go built. The
// URL must be https:// with a host and without userinfo or fragment; an
// existing query string is kept but must not already set [EgressProxyURLParam].
//
// Callers building their own transport from [NewSSRFSafeTransport] are not
// covered: only the shared transports behind [TransportForAllowPrivateIPs]
// and [ClientForAllowPrivateIPs] (MCP, fetch, api, openapi, a2a) honour it.
func WithEgressProxy(ctx context.Context, proxyURL string) (context.Context, error) {
	u, err := parseEgressProxyURL(proxyURL)
	if err != nil {
		return nil, err
	}
	return context.WithValue(ctx, egressProxyKey{}, u), nil
}

// EgressProxyFromContext returns the egress proxy URL set by
// [WithEgressProxy], or nil when none is configured.
func EgressProxyFromContext(ctx context.Context) *url.URL {
	u, _ := ctx.Value(egressProxyKey{}).(*url.URL)
	if u == nil {
		return nil
	}
	clone := *u
	return &clone
}

func parseEgressProxyURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid egress proxy URL: %w", err)
	}
	switch {
	case u.Scheme != "https":
		return nil, fmt.Errorf("egress proxy URL %q must use https", u.Redacted())
	case u.Host == "" || u.Hostname() == "":
		return nil, fmt.Errorf("egress proxy URL %q must have a host", u.Redacted())
	case u.User != nil:
		return nil, fmt.Errorf("egress proxy URL %q must not contain userinfo", u.Redacted())
	case u.Fragment != "" || u.RawFragment != "":
		return nil, fmt.Errorf("egress proxy URL %q must not contain a fragment", u.Redacted())
	case u.Query().Has(EgressProxyURLParam):
		return nil, fmt.Errorf("egress proxy URL %q must not set the %q query parameter", u.Redacted(), EgressProxyURLParam)
	}
	return u, nil
}

// egressProxyTransport is the guarded transport on browser builds. It
// rewrites every request to target the egress proxy from the request context,
// carrying the original absolute URL in [EgressProxyURLParam], and fails
// closed with [ErrBrowserEgressUnsupported] when no proxy is configured.
// base performs the rewritten request.
type egressProxyTransport struct {
	base http.RoundTripper
}

func (t *egressProxyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	proxy := EgressProxyFromContext(req.Context())
	if proxy == nil {
		return nil, ErrBrowserEgressUnsupported
	}
	if req.URL == nil || (req.URL.Scheme != "http" && req.URL.Scheme != "https") || req.URL.Host == "" {
		return nil, fmt.Errorf("egress proxy requires an absolute http(s) target URL, got %q", redacted(req.URL))
	}
	// Like a direct request, the fragment is never sent on the wire.
	target := *req.URL
	target.Fragment, target.RawFragment = "", ""

	query := proxy.Query()
	query.Set(EgressProxyURLParam, target.String())
	proxy.RawQuery = query.Encode()

	out := req.Clone(req.Context())
	out.URL = proxy
	// The Host header must name the proxy, not the target.
	out.Host = ""
	// Consumed by net/http's fetch path: the browser must never follow a
	// redirect off the proxy, which would bypass its destination checks.
	// Redirects come back as data in EgressProxyLocationHeader instead.
	out.Header.Set("js.fetch:redirect", "error")
	// The only credential is the explicit Authorization header; the page's
	// cookies for the proxy origin must not be attached, nor Set-Cookie honoured.
	out.Header.Set("js.fetch:credentials", "omit")

	resp, err := t.base.RoundTrip(out)
	if err != nil {
		return nil, err
	}
	// Callers resolve relative links and report the final URL from
	// resp.Request; it must name the target, not the proxy.
	resp.Request = req
	if err := restoreEgressLocation(resp); err != nil {
		if resp.Body != nil {
			resp.Body.Close()
		}
		return nil, err
	}
	return resp, nil
}

// restoreEgressLocation moves the proxy's relayed Location back into the
// Location header on redirect responses. The metadata header is always
// removed so it cannot leak to callers; on non-redirect statuses it is
// ignored, so the proxy cannot turn a 200 into a redirect.
func restoreEgressLocation(resp *http.Response) error {
	loc := resp.Header.Get(EgressProxyLocationHeader)
	resp.Header.Del(EgressProxyLocationHeader)
	if !isRedirectStatus(resp.StatusCode) {
		return nil
	}
	if location := resp.Header.Get("Location"); location != "" {
		// Browsers already fail such a fetch under redirect: "error"; keep
		// Node and native mocks equally strict.
		return fmt.Errorf("egress proxy returned %d with a Location header %q; it must relay redirects in %s",
			resp.StatusCode, location, EgressProxyLocationHeader)
	}
	if loc != "" {
		resp.Header.Set("Location", loc)
	}
	return nil
}

// isRedirectStatus matches the statuses http.Client and fetch treat as
// redirects.
func isRedirectStatus(code int) bool {
	switch code {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

func redacted(u *url.URL) string {
	if u == nil {
		return ""
	}
	return u.Redacted()
}
