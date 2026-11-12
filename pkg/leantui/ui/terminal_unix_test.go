//go:build !windows

package ui

import (
	"testing"

	"github.com/creack/pty"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/term"
)

func TestTerminalSizeReadsCurrentDimensions(t *testing.T) {
	t.Parallel()

	master, tty, err := pty.Open()
	require.NoError(t, err)
	t.Cleanup(func() { _ = master.Close(); _ = tty.Close() })
	outputMaster, outputTTY, err := pty.Open()
	require.NoError(t, err)
	t.Cleanup(func() { _ = outputMaster.Close(); _ = outputTTY.Close() })
	require.NoError(t, pty.Setsize(master, &pty.Winsize{Cols: 200, Rows: 60}))
	initialState, err := term.GetState(int(tty.Fd()))
	require.NoError(t, err)
	terminal, err := NewTerminal(tty, outputTTY)
	require.NoError(t, err)
	t.Cleanup(func() {
		terminal.Restore()
		restoredState, err := term.GetState(int(tty.Fd()))
		require.NoError(t, err)
		assert.Equal(t, initialState, restoredState)
	})

	for _, size := range []pty.Winsize{
		{Cols: 80, Rows: 24},
		{Cols: 120, Rows: 40},
		{Cols: 20, Rows: 5},
		{Cols: 1, Rows: 1},
		{Cols: 0, Rows: 40},
		{Cols: 120, Rows: 0},
	} {
		require.NoError(t, pty.Setsize(outputMaster, &size))
		w, h := terminal.Size()
		wantW, wantH := normalizeTerminalSize(int(size.Cols), int(size.Rows))
		assert.Equal(t, wantW, w)
		assert.Equal(t, wantH, h)
	}
}
