package mcp

import (
	"os"
	"testing"
	"time"

	"github.com/docker/docker-agent/pkg/httpclient"
)

// TestMain swaps the OAuth helpers' SSRF-safe HTTP client for the
// loopback-allowing variant so tests can hit httptest.NewServer (which
// binds to 127.0.0.1). Production code keeps the safe client.
func TestMain(m *testing.M) {
	oauthHTTPClient = httpclient.NewSafeClient(30*time.Second, true)
	os.Exit(m.Run())
}
