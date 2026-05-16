package teamloader

import (
	"os"
	"testing"

	"github.com/docker/docker-agent/pkg/gateway"
)

// TestMain seeds a fake MCP catalog so that teamloader tests that invoke
// createMCPTool with a ref can run without a live network call.
func TestMain(m *testing.M) {
	gateway.OverrideCatalogForTesting(gateway.Catalog{
		// A local (subprocess-based) server entry.
		"local-server": {
			Type: "server",
		},
		// A remote (no subprocess) server entry — used to test that
		// working_dir is rejected at runtime for ref-based remote MCPs.
		"remote-server": {
			Type: "remote",
			Remote: gateway.Remote{
				URL:           "https://mcp.example.com/sse",
				TransportType: "sse",
			},
		},
	})
	os.Exit(m.Run())
}
