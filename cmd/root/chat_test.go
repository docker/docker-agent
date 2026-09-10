package root

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChatRejectsUnauthenticatedNonLoopbackBind(t *testing.T) {
	t.Parallel()

	cmd := newChatCmd()
	cmd.SetArgs([]string{"agent.yaml", "--listen", "0.0.0.0:8083"})
	err := cmd.ExecuteContext(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "require --api-key, --api-key-env, or --insecure-no-auth")
}

func TestChatRejectsEmptyAPIKeyEnvironmentVariable(t *testing.T) {
	t.Setenv("CHAT_API_KEY", "")

	cmd := newChatCmd()
	cmd.SetArgs([]string{"agent.yaml", "--api-key-env", "CHAT_API_KEY"})
	err := cmd.ExecuteContext(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CHAT_API_KEY")
}
