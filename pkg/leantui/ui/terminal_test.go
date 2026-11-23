package ui

import (
	"bufio"
	"bytes"
	"os"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeTerminalSize(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name          string
		width, height int
		wantW, wantH  int
	}{
		{name: "valid", width: 120, height: 40, wantW: 120, wantH: 40},
		{name: "zero", wantW: 80, wantH: 24},
		{name: "negative", width: -1, height: -1, wantW: 80, wantH: 24},
		{name: "width only", height: 40, wantW: 80, wantH: 40},
		{name: "height only", width: 120, wantW: 120, wantH: 24},
		{name: "tiny", width: 1, height: 1, wantW: 1, wantH: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w, h := normalizeTerminalSize(tc.width, tc.height)
			assert.Equal(t, tc.wantW, w)
			assert.Equal(t, tc.wantH, h)
		})
	}
}

func TestTerminalSizeWithoutTTY(t *testing.T) {
	t.Parallel()

	out, err := os.Open(os.DevNull)
	require.NoError(t, err)
	t.Cleanup(func() { _ = out.Close() })
	terminal := &Terminal{out: out}

	w, h := terminal.Size()
	assert.Equal(t, 80, w)
	assert.Equal(t, 24, h)
}

func TestTerminalSetWindowTitle(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	terminal := &Terminal{writer: bufio.NewWriter(&buf)}

	terminal.SetWindowTitle("docker agent: Fix auth bug")
	assert.Equal(t, ansi.SetWindowTitle("docker agent: Fix auth bug"), buf.String())

	buf.Reset()
	terminal.SetWindowTitle("docker agent: Line 1\nLine 2\x07\x1b]0;injected")
	assert.Equal(t, ansi.SetWindowTitle("docker agent: Line 1 Line 2]0;injected"), buf.String())

	var nilTerm *Terminal
	assert.NotPanics(t, func() {
		nilTerm.SetWindowTitle("ignored")
		(&Terminal{}).SetWindowTitle("ignored")
	})
}
