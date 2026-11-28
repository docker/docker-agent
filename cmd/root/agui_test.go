package root

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAGUIRejectsUnauthenticatedNonLoopbackBind(t *testing.T) {
	t.Parallel()

	cmd := newAGUICmd()
	cmd.SetArgs([]string{"agent.yaml", "--listen", "0.0.0.0:8084"})
	err := cmd.ExecuteContext(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "require --auth-token or --insecure-no-auth")
}

func TestAGUIRejectsInvalidCORSOrigin(t *testing.T) {
	t.Parallel()

	cmd := newAGUICmd()
	cmd.SetArgs([]string{"agent.yaml", "--cors-origin", "~("})
	err := cmd.ExecuteContext(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid --cors-origin")
}

func TestAGUIRejectsInvalidSafety(t *testing.T) {
	t.Parallel()

	cmd := newAGUICmd()
	cmd.SetArgs([]string{"agent.yaml", "--safety", "reckless"})
	require.Error(t, cmd.ExecuteContext(t.Context()))
}
