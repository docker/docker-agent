//go:build js

package httpclient

import (
	"net/http"
	"strings"
	"syscall/js"
)

// wrapBrowserEgress replaces guarded transports with the egress-proxy
// transport in the browser, where net/http serves requests through fetch and
// the dial-time SSRF guard never runs. Unguarded transports (explicit
// allow_private_ips opt-ins) and Node processes — whose net/http uses the
// regular dialing round trip — keep rt.
func wrapBrowserEgress(rt http.RoundTripper, guarded bool) http.RoundTripper {
	return wrapBrowserEgressWith(rt, guarded, isNodeProcess(), &http.Transport{})
}

// wrapBrowserEgressWith is the injectable core of wrapBrowserEgress. fetch
// must be a transport without Dial funcs so net/http routes it through the
// browser's fetch API.
func wrapBrowserEgressWith(rt http.RoundTripper, guarded, node bool, fetch http.RoundTripper) http.RoundTripper {
	if !guarded || node {
		return rt
	}
	return &egressProxyTransport{base: fetch}
}

// isNodeProcess mirrors net/http's own Node detection (jsFetchDisabled).
func isNodeProcess() bool {
	process := js.Global().Get("process")
	return process.Type() == js.TypeObject && strings.HasPrefix(process.Get("argv0").String(), "node")
}
