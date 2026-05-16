package httpclient

import (
	"net/http"
	"time"
)

// NewSafeClient returns the HTTP client used by built-in tools that issue
// outbound calls to URLs the operator (or a fetched OpenAPI spec) supplies.
//
// The default refuses connections to non-public IPs at dial time
// — defeating DNS rebinding to loopback / RFC1918 / link-local incl. cloud
// metadata at 169.254.169.254 — and bounds the redirect chain at 10 hops.
//
// When unsafe is true the client uses [http.DefaultTransport]. This branch
// exists ONLY for tests, which use [httptest.NewServer] (binds to 127.0.0.1)
// and therefore cannot pass the SSRF check.
func NewSafeClient(timeout time.Duration, unsafe bool) *http.Client {
	if unsafe {
		return &http.Client{Timeout: timeout}
	}
	return &http.Client{
		Timeout:       timeout,
		Transport:     NewSSRFSafeTransport(),
		CheckRedirect: BoundedRedirects(10),
	}
}
