package root

import (
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
