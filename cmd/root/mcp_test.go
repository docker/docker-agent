package root

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMCPHTTPSafetyAndAuthenticationFlagsRequireHTTP(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{
		{"agent.yaml", "--safety", "restricted"},
		{"agent.yaml", "--auth-token", "secret"},
		{"agent.yaml", "--insecure-no-auth"},
		{"--attach", "--safety", "restricted"},
	} {
		cmd := newMCPCmd()
		cmd.SetArgs(args)
		err := cmd.ExecuteContext(t.Context())
		require.Error(t, err)
		if len(args) > 0 && args[0] == "--attach" {
			assert.Contains(t, err.Error(), "--http-only")
		} else {
			assert.Contains(t, err.Error(), "require --http")
		}
	}
}

func TestMCPHTTPRejectsUnauthenticatedNonLoopbackBind(t *testing.T) {
	t.Parallel()

	cmd := newMCPCmd()
	cmd.SetArgs([]string{"agent.yaml", "--http", "--listen", "0.0.0.0:8081"})
	err := cmd.ExecuteContext(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "require --auth-token or --insecure-no-auth")
}

func TestMCPKeepAliveRejectedWithHTTP(t *testing.T) {
	t.Parallel()

	// The stateless HTTP transport (MCP 2026-07-28) rejects server-initiated
	// requests, so a keep-alive ping interval must be refused before the
	// server starts listening.
	cmd := newMCPCmd()
	cmd.SetArgs([]string{"agent.yaml", "--http", "--mcp-keepalive", "30s"})
	err := cmd.ExecuteContext(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--mcp-keepalive")
	assert.Contains(t, err.Error(), "stateless HTTP")
}

func TestMCPKeepAliveRejectedWithAttach(t *testing.T) {
	t.Parallel()

	// --attach proxies a running TUI session through its own MCP server,
	// which never sees the runtime configuration: the flag must be refused
	// rather than silently ignored.
	cmd := newMCPCmd()
	cmd.SetArgs([]string{"--attach", "--mcp-keepalive", "30s"})
	err := cmd.ExecuteContext(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--mcp-keepalive")
	assert.Contains(t, err.Error(), "--attach")
}

func TestMCPKeepAliveAcceptedForStdio(t *testing.T) {
	t.Parallel()

	// An unparsable agent file makes the stdio path fail during config
	// loading, which is past flag validation: the keep-alive flag itself
	// must not be rejected without --http.
	agentFile := filepath.Join(t.TempDir(), "broken.yaml")
	require.NoError(t, os.WriteFile(agentFile, []byte("not: [valid"), 0o600))

	cmd := newMCPCmd()
	cmd.SetArgs([]string{agentFile, "--mcp-keepalive", "30s"})
	err := cmd.ExecuteContext(t.Context())
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "--mcp-keepalive")
}
