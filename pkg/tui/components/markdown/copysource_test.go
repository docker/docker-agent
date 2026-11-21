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
		{"control", "```\nfirst\x1b[31msecond\n```", "first\x1b[31msecond"},
		{"crlf", "```\r\none\r\ntwo\r\n```", "one\r\ntwo"},
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
		"```\nfoo\rbar\n```",
	} {
		view, blocks, err := NewFastRenderer(60).RenderWithSourceCodeBlocks(input)
		require.NoError(t, err)
		assert.Empty(t, blocks)
		assert.NotContains(t, view, CodeBlockCopyIcon)
		assert.NotContains(t, ansi.Strip(view), "hidden")
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
