//go:build !windows

package cli

import (
	"bytes"
	"os"
	"testing"

	"github.com/creack/pty"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

func TestRunLastRequiresPromptWithTerminalStdin(t *testing.T) {
	master, tty, err := pty.Open()
	require.NoError(t, err)
	t.Cleanup(func() { _ = master.Close(); _ = tty.Close() })
	old := os.Stdin
	os.Stdin = tty
	t.Cleanup(func() { os.Stdin = old })
	var out bytes.Buffer
	err = Run(t.Context(), NewPrinter(&out), Config{Last: true, OutputJSON: true}, &mockRuntime{}, session.New(), nil)
	require.ErrorContains(t, err, "requires a message")
	assert.Empty(t, out.String())
}
