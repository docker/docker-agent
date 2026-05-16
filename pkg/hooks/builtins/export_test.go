package builtins

import (
	"time"

	"github.com/docker/docker-agent/pkg/httpclient"
)

// SetHTTPPostClientUnsafeForTest swaps the httpPost client for one that
// bypasses SSRF dial-time protection so tests can talk to
// httptest.NewServer (which binds to 127.0.0.1). Returns a restore
// function. Test-only — this file is *_test.go so it never compiles
// into release binaries.
func SetHTTPPostClientUnsafeForTest() func() {
	prev := httpPostClient
	httpPostClient = httpclient.NewSafeClient(30*time.Second, true)
	return func() { httpPostClient = prev }
}
