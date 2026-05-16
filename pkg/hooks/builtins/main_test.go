package builtins_test

import (
	"os"
	"testing"

	"github.com/docker/docker-agent/pkg/hooks/builtins"
)

// TestMain flips the http_post client to its unsafe variant for the
// duration of this test binary, since httptest.NewServer binds to
// 127.0.0.1 and is otherwise rejected by the production SSRF dialer.
// Production callers always go through the safe client wired in
// http_post.go.
func TestMain(m *testing.M) {
	builtins.SetHTTPPostClientUnsafeForTest()
	os.Exit(m.Run())
}
