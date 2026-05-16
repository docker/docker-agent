package lsp

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools/lifecycle"
)

// TestLSPTool_StartFailureWhenServerMissing verifies that Start returns
// the typed lifecycle.ErrServerUnavailable when the LSP binary doesn't
// exist, so the runtime / supervisor can apply the right policy.
func TestLSPTool_StartFailureWhenServerMissing(t *testing.T) {
	t.Parallel()

	// Use a binary path that surely does not exist anywhere on PATH.
	tool := NewLSPTool("docker-agent-lsp-does-not-exist", nil, nil, t.TempDir())
	err := tool.Start(t.Context())
	require.Error(t, err)
	assert.ErrorIs(t, err, lifecycle.ErrServerUnavailable)
}

// TestLSPTool_StopBeforeStart verifies Stop is idempotent and does not
// fail when called before Start (or twice).
func TestLSPTool_StopBeforeStart(t *testing.T) {
	t.Parallel()

	tool := NewLSPTool("docker-agent-lsp-does-not-exist", nil, nil, t.TempDir())
	require.NoError(t, tool.Stop(t.Context()))
	require.NoError(t, tool.Stop(t.Context()))
}

// TestLSPTool_SupervisorRetryAfterFailure verifies that a Start failure
// leaves the supervisor in StateStopped so the runtime can retry.
// (The previous behaviour required Start to clear internal state on
// failure; the supervisor handles this for us now.)
func TestLSPTool_SupervisorRetryAfterFailure(t *testing.T) {
	t.Parallel()

	tool := NewLSPTool("docker-agent-lsp-does-not-exist", nil, nil, t.TempDir())

	// First attempt: Start fails because the binary is missing.
	err := tool.Start(t.Context())
	require.Error(t, err)

	// State should be Stopped (not Failed), so the runtime can retry.
	state := tool.handler.supervisor.State()
	assert.Equal(t, lifecycle.StateStopped, state.State)

	// Second attempt: Start fails the same way; the supervisor remains
	// retryable.
	err = tool.Start(t.Context())
	require.Error(t, err)
	state = tool.handler.supervisor.State()
	assert.Equal(t, lifecycle.StateStopped, state.State)

	// Allow some time to ensure no goroutine leaked or panicked.
	time.Sleep(50 * time.Millisecond)
}
