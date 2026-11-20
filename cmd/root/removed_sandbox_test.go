package root

import (
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRemovedSandboxCommandsAndFlags(t *testing.T) {
	t.Parallel()

	for _, flag := range []string{"--sandbox", "--sbx", "--template", "--no-kit", "--cloud", "--sandbox-kit", "--kit", "--kit-arg", "--sandbox-ttl"} {
		require.ErrorContains(t, newRunCmd().ParseFlags([]string{flag}), "unknown flag: "+flag)
	}
	require.ErrorContains(t, newAliasAddCmd().ParseFlags([]string{"--sandbox"}), "unknown flag: --sandbox")

	cmd := NewRootCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"sandbox"})
	require.ErrorContains(t, cmd.ExecuteContext(t.Context()), `unknown command "sandbox"`)
}
