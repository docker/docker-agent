//go:build !js

package httpclient

import "net/http"

// wrapBrowserEgress is the identity on native builds: the direct transport
// enforces the SSRF guard at dial time.
func wrapBrowserEgress(rt http.RoundTripper, _ bool) http.RoundTripper {
	return rt
}
