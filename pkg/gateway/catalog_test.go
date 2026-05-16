package gateway

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testCatalog is a self-contained catalog used by all tests, removing the
// dependency on the live Docker MCP catalog and the network.
var testCatalog = Catalog{
	"github-official": {
		Type: "server",
		Secrets: []Secret{
			{Name: "github.personal_access_token", Env: "GITHUB_PERSONAL_ACCESS_TOKEN"},
		},
	},
	"fetch": {
		Type: "server",
	},
	"apify": {
		Type: "remote",
		Secrets: []Secret{
			{Name: "apify.token", Env: "APIFY_TOKEN"},
		},
		Remote: Remote{
			URL:           "https://mcp.apify.com",
			TransportType: "streamable-http",
		},
	},
}

func TestMain(m *testing.M) {
	// Override the production catalogOnce so that tests never hit the network.
	catalogOnce = func() (Catalog, error) {
		return testCatalog, nil
	}
	os.Exit(m.Run())
}

func TestRequiredEnvVars_local(t *testing.T) {
	secrets, err := RequiredEnvVars(t.Context(), "github-official")
	require.NoError(t, err)

	assert.Len(t, secrets, 1)
	assert.Equal(t, "GITHUB_PERSONAL_ACCESS_TOKEN", secrets[0].Env)
	assert.Equal(t, "github.personal_access_token", secrets[0].Name)
}

func TestRequiredEnvVars_remote(t *testing.T) {
	secrets, err := RequiredEnvVars(t.Context(), "apify")
	require.NoError(t, err)

	assert.Empty(t, secrets)
}

func TestServerSpec_local(t *testing.T) {
	server, err := ServerSpec(t.Context(), "fetch")
	require.NoError(t, err)

	assert.Equal(t, "server", server.Type)
}

func TestServerSpec_remote(t *testing.T) {
	server, err := ServerSpec(t.Context(), "apify")
	require.NoError(t, err)

	assert.Equal(t, "remote", server.Type)
	assert.Equal(t, "https://mcp.apify.com", server.Remote.URL)
	assert.Equal(t, "streamable-http", server.Remote.TransportType)
}

func TestServerSpec_notFound(t *testing.T) {
	_, err := ServerSpec(t.Context(), "nonexistent")
	require.Error(t, err)

	assert.Contains(t, err.Error(), "not found in MCP catalog")
}

func TestParseServerRef(t *testing.T) {
	assert.Equal(t, "github-official", ParseServerRef("docker:github-official"))
	assert.Equal(t, "github-official", ParseServerRef("github-official"))
}
