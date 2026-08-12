package root

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAPISessionWorkingDirRootFlag: serve api exposes the opt-in containment
// root for session working directories, unrestricted by default.
func TestAPISessionWorkingDirRootFlag(t *testing.T) {
	t.Parallel()

	cmd := newAPICmd()

	flag := cmd.PersistentFlags().Lookup("session-workingdir-root")
	require.NotNil(t, flag, "serve api must expose --session-workingdir-root")
	assert.Empty(t, flag.DefValue, "default must be unrestricted")
	assert.False(t, flag.Hidden)

	require.NoError(t, cmd.PersistentFlags().Parse([]string{"--session-workingdir-root", "/srv/workspaces"}))
	value, err := cmd.PersistentFlags().GetString("session-workingdir-root")
	require.NoError(t, err)
	assert.Equal(t, "/srv/workspaces", value)
}

// TestRunSessionWorkingDirRootFlagIsHidden: run exposes the same containment
// root for its --listen control plane, hidden like --listen itself.
func TestRunSessionWorkingDirRootFlagIsHidden(t *testing.T) {
	t.Parallel()

	cmd := newRunCmd()

	flag := cmd.PersistentFlags().Lookup("session-workingdir-root")
	require.NotNil(t, flag, "run must expose --session-workingdir-root for --listen")
	assert.Empty(t, flag.DefValue, "default must be unrestricted")
	assert.True(t, flag.Hidden, "advanced/automation flag stays hidden like --listen")
}

// TestValidateSessionWorkingDirRoot: a value that trims to empty must fail
// loudly instead of silently disabling containment.
func TestValidateSessionWorkingDirRoot(t *testing.T) {
	t.Parallel()

	require.NoError(t, validateSessionWorkingDirRoot(""))
	require.NoError(t, validateSessionWorkingDirRoot("/srv/workspaces"))

	err := validateSessionWorkingDirRoot("   ")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--session-workingdir-root")

	require.Error(t, validateSessionWorkingDirRoot("\t\n"))
}
