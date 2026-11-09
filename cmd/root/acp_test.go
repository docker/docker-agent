package root

import (
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestACPArgs(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{name: "default"},
		{name: "builtin", args: []string{"coder"}},
		{name: "file", args: []string{"agent.yaml"}},
		{name: "registry", args: []string{"myorg/agent:tag"}},
		{name: "too many", args: []string{"agent.yaml", "other.yaml"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := newACPCmd().ValidateArgs(tc.args)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestACPWithoutAgent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	sessionDB := filepath.Join(t.TempDir(), "session.db")

	cmd := newACPCmd()
	cmd.SetArgs([]string{"--session-db", sessionDB})
	cmd.SetIn(strings.NewReader(""))
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)

	require.NoError(t, cmd.ExecuteContext(t.Context()))
	assert.FileExists(t, sessionDB)
}
