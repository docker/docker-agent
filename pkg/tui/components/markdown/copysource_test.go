package markdown

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLongFencesAndSourceCode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, message, want string }{
		{"backticks", "````sh\none\n```\ntwo\n````", "one\n```\ntwo"},
		{"tildes", "~~~~sh\none\n~~~\ntwo\n~~~~", "one\n~~~\ntwo"},
		{"quote", "> ````\n> one\n> ```\n> two\n> ````", "one\n```\ntwo"},
		{"list", "- commands\n  ````\n  one\n  ```\n  two\n  ````", "one\n```\ntwo"},
		{"crlf", "```\r\none\r\ntwo\r\n```", "one\r\ntwo"},
		{"crlf after prose", "some prose\r\n\r\n```sh\r\none\r\ntwo\r\n```", "one\r\ntwo"},
		{"crlf blank last line", "```\r\none\r\n\r\n```", "one\r\n"},
		{"crlf quote", "> > ```\r\n> > a  \r\n> > b\r\n> > ```", "a  \r\nb"},
		{"crlf list", "- step\r\n    ~~~\r\n  echo hello\r\n  echo bye\r\n    ~~~", "echo hello\r\necho bye"},
		{"crlf unclosed", "```\r\none\r\n", "one\r\n"},
		{"mixed line endings", "prose\r\n\n```\nfirst\r\nsecond\nthird\r\n```", "first\r\nsecond\nthird"},
		{"list short indent", "- step\n    ~~~\n  echo hello\n    ~~~", "echo hello"},
		{"list tabs", "- step\n\t~~~\n\techo hello\n\t~~~", "echo hello"},
		{"list tab crossing boundary", "- step\n  ~~~\n\techo hello\n  ~~~", "  echo hello"},
		{"list spaces for tab", "- step\n\t~~~\n    echo hello\n\t~~~", "echo hello"},
		{"nested quote whitespace", "> > ```\n> >   one  \n> > two\t \n> > ```", "  one  \ntwo\t "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view, blocks, err := NewFastRenderer(60).RenderWithSourceCodeBlocks(tc.message)
			require.NoError(t, err)
			require.Len(t, blocks, 1)
			assert.Equal(t, tc.want, blocks[0].Content)
			assert.NotContains(t, ansi.Strip(view), "\x1b")
		})
	}
}

func TestSourceCopyHidesAmbiguousBlocks(t *testing.T) {
	t.Parallel()
	// An OSC sequence can span what looks like a second fenced block.
	for _, input := range []string{
		"```\nsafe\n```\n\n\x1b]0;\n```\nhidden\n```\x07",
		"```\necho hi\x1b]0;\rid\r\x07\n```",
		"```\necho hi\x1bPpayload\x1b\\\n```",
		"```\nprintf '%s' sa\x1b[31mfe\n```",
		"```\nfoo\rbar\n```",
		"```\nfoo\r",
		"```\nfoo\r\nbar\r",
		"```\na\n```\n\n```\nb\x1b[0m\n```",
		"\x1b[31mprose\x1b[0m\n\n```\nsafe\n```",
	} {
		for _, trusted := range []bool{false, true} {
			r := NewFastRenderer(60)
			if trusted {
				r.AllowSGR()
			}
			view, blocks, err := r.RenderWithSourceCodeBlocks(input)
			require.NoError(t, err)
			assert.Empty(t, blocks, "trusted=%v input=%q", trusted, input)
			assert.NotContains(t, view, CodeBlockCopyIcon)
			assert.NotContains(t, ansi.Strip(view), "hidden")
		}
	}
}

func TestContainsHiddenControl(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, input string
		want        bool
	}{
		{"unicode", "日本語🙂", false},
		{"crlf", "a\r\nb", false},
		{"sgr", "a\x1b[31mb", true},
		{"raw c1", "a\x9bb", true},
		{"encoded c1", "a\u009bb", true},
		{"standalone cr", "a\rb", true},
		{"invalid utf8", "a\xff", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, containsHiddenControl(tc.input))
		})
	}
}

func TestLongFenceIncrementalMatchesFullRender(t *testing.T) {
	t.Parallel()
	input := "````\nfirst\n```\n\nsecond\n````\n\nafter"
	r := NewIncrementalRenderer(60)
	for end := range len(input) + 1 {
		got, blocks, err := r.RenderWithCodeBlocks(input[:end])
		require.NoError(t, err)
		want, wantBlocks, err := NewFastRenderer(60).RenderWithCodeBlocks(input[:end])
		require.NoError(t, err)
		assert.Equal(t, want, got, "at %d", end)
		assert.Equal(t, wantBlocks, blocks, "at %d", end)
	}
}

func TestSourceCopyTrustedSGRDisplaySafety(t *testing.T) {
	t.Parallel()
	for _, control := range []string{"\x1b[32m", "\x9b32m", "\u009b32m", "\x1b]52;c;cHduZWQ=\x07", "\x1b[2J"} {
		for _, input := range []string{
			control + "prose\n\n```\nsafe\n```",
			"```\n" + control + "safe\n```",
			"```\nsafe\n```\n\n" + control + "prose",
		} {
			r := NewFastRenderer(60).AllowSGR()
			want, err := NewFastRenderer(60).AllowSGR().HideCopyIcon().Render(input)
			require.NoError(t, err)
			view, blocks, err := r.RenderWithSourceCodeBlocks(input)
			require.NoError(t, err)
			assert.Equal(t, want, view, "trusted styling remains visible without copy labels")
			assert.Empty(t, blocks)
			assert.NotContains(t, view, CodeBlockCopyIcon)
			assert.NotContains(t, view, "\x1b]52;")
			assert.NotContains(t, view, "\x1b[2J")

			view, blocks, err = r.RenderWithSourceCodeBlocks("```\r\none\r\ntwo\r\n```")
			require.NoError(t, err)
			require.Len(t, blocks, 1, "the renderer still permits copying control-free source")
			assert.Equal(t, "one\r\ntwo", blocks[0].Content)
			assert.Contains(t, view, CodeBlockCopyIcon)
		}
	}
	view, _, err := NewFastRenderer(60).AllowSGR().RenderWithSourceCodeBlocks("\x1b[32mprose\x1b[0m\n\n```\nsafe\n```")
	require.NoError(t, err)
	assert.Contains(t, view, "\x1b[32mprose\x1b[0m")
}
