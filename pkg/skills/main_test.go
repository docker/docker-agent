package skills

import (
	"net/http"
	"os"
	"testing"

	"github.com/docker/docker-agent/pkg/httpclient"
)

// TestMain swaps the SSRF-safe HTTP client for the loopback-allowing
// variant so tests can hit httptest.NewServer (which binds to 127.0.0.1).
// Its private pool is unaffected by parallel httptest server cleanup.
// Production code keeps the safe client.
func TestMain(m *testing.M) {
	skillsHTTPClient = httpclient.NewSafeClient(remoteHTTPTimeout, true)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	skillsHTTPClient.Transport = transport
	code := m.Run()
	transport.CloseIdleConnections()
	os.Exit(code)
}
