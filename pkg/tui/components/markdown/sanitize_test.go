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
	assert.Equal(t, "abcd\n\te", StripTerminalControls("a\x07b\u0085c\x7fd\u009b2J\n\te"),
		"C0, DEL and C1 controls are removed, C1 sequences with their parameters")
	assert.Equal(t, "before", StripTerminalControls("before\x1bPunterminated payload"), "an unterminated control string runs to the end")
	assert.Equal(t, "ab", StripTerminalControls("a\x1b]0;title\x07b"), "BEL ends an OSC before control characters are removed")
}

func TestIncrementalRendererStripsControlStringsAcrossBlocks(t *testing.T) {
	t.Parallel()
	for _, source := range []string{
		"before\x1bPsecret\n\npayload\x1b\\after\n\nnext",
		"before\x1bPsecret\n\npayload never terminated\n\nmore",
		"before\x1b]0;title\n\nstill title\x07after",
	} {
		want, err := NewFastRenderer(40).Render(source)
		require.NoError(t, err)
		got, err := NewIncrementalRenderer(40).Render(source)
		require.NoError(t, err)
		assert.Equal(t, want, got, "%q", source)

		streamed := NewIncrementalRenderer(40)
		for i := 1; i <= len(source); i++ {
			got, err = streamed.Render(source[:i])
			require.NoError(t, err)
			assert.NotContains(t, ansi.Strip(got), "payload", "%q at %d", source, i)
			assert.NotContains(t, ansi.Strip(got), "title", "%q at %d", source, i)
		}
		assert.Equal(t, want, got, "streamed %q", source)
	}
}

func TestStripTerminalControlsHandlesBothC1Encodings(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, in, want string }{
		{"raw CSI", "file\x9b2J.png", "file.png"},
		{"encoded CSI", "file\u009b2J.png", "file.png"},
		{"raw OSC 52", "a\x9d52;c;cHduZWQ=\x9cb", "ab"},
		{"encoded OSC 52", "a\u009d52;c;cHduZWQ=\u009cb", "ab"},
		{"raw DCS", "a\x90payload\x9cb", "ab"},
		{"mixed terminator", "a\x9d0;title\x1b\\b", "ab"},
		{"raw NEL", "a\x85b", "ab"},
		{"other invalid UTF-8", "a\xffb\xc2", "a\ufffdb\ufffd"},
		{"continuation bytes in valid text", "✓ › \u00a0 é 日本", "✓ › \u00a0 é 日本"},
	} {
		assert.Equal(t, tc.want, StripTerminalControls(tc.in), tc.name)
	}
}
