package markdown

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRendererStripsTerminalControls(t *testing.T) {
	t.Parallel()
	for _, control := range []string{
		"\x1b[2J",
		"\x1b[?25l",
		"\x1b]52;c;cHduZWQ=\x07",
		"\x1b]0;title\x1b\\",
		"\x1b7",
		"\x1b(B",
		"\x1bPpayload\x1b\\",
		"\x1b_apc payload\x1b\\",
		"\x1b[32m",
	} {
		rendered, err := NewFastRenderer(80).Render("before " + control + " after")
		require.NoError(t, err)
		assert.Equal(t, "before  after", strings.TrimSpace(ansi.Strip(rendered)), "%q", control)
		assert.NotContains(t, rendered, control)
	}
	rendered, err := NewFastRenderer(80).Render("[link](https://example.com)")
	require.NoError(t, err)
	assert.Contains(t, rendered, "\x1b]8;;https://example.com", "renderer-generated hyperlinks survive")
}

func TestStripTerminalControls(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "a b", StripTerminalControls("a\x1b[31m \x1b[2Jb\r"))
	assert.Equal(t, "abcd2J\n\te", StripTerminalControls("a\x07b\u0085c\x7fd\u009b2J\n\te"),
		"C0, DEL and C1 controls are removed; a lone C1 CSI leaves inert parameters")
	assert.Equal(t, "before", StripTerminalControls("before\x1bPunterminated payload"), "an unterminated control string runs to the end")
	assert.Equal(t, "ab", StripTerminalControls("a\x1b]0;title\x07b"), "BEL ends an OSC before control characters are removed")
}
