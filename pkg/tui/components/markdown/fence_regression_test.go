package markdown

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClosingFenceRequiresContiguousCharacters(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		line, fence string
		want        bool
	}{
		{"~~~", "~~~", true},
		{"  ~~~~~ \t", "~~~", true},
		{"~~", "~~~", false},
		{"~~~ ~~~", "~~~", false},
		{"~~~\t~", "~~~", false},
		{"~~~ text", "~~~", false},
		{"```` \t", "```", true},
		{"``` `", "```", false},
	} {
		assert.Equal(t, tc.want, closingFence(tc.line, tc.fence), "%q", tc.line)
	}
}

func TestFenceCopyRegression(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, input, want string }{
		{"zero indent list", "- step\n~~~\necho hi\n  spaced\n\n~~~", "echo hi\n  spaced\n"},
		{"unicode nested quote", "> \u00a0> ~~~\n> \u2003> echo  \n> \u00a0> next\t \n> \u00a0> ~~~", "echo  \nnext\t "},
		{"separated fence", "~~~\none\n~~~ ~~~\ntwo\n~~~~", "one\n~~~ ~~~\ntwo"},
		{"separated quote fence", "> ~~~\n> one\n> ~~~ ~~~\n> two\n> ~~~~", "one\n~~~ ~~~\ntwo"},
		{"separated list fence", "- step\n  ```\n  one\n  ``` `\n  two\n  ````", "one\n``` `\ntwo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view, blocks, err := NewFastRenderer(60).RenderWithSourceCodeBlocks(tc.input)
			require.NoError(t, err)
			require.Len(t, blocks, 1)
			assert.Equal(t, tc.want, blocks[0].Content)
			first, _, _ := strings.Cut(tc.want, "\n")
			assert.Contains(t, ansi.Strip(view), strings.TrimSpace(first))
		})
	}
}

func TestUnicodeFenceIncrementalMatchesFullRender(t *testing.T) {
	t.Parallel()
	for _, input := range []string{
		"intro\n\u00a0~~~\ninside\n~~~\n\nafter",
		"intro\n\n\u2003````\none\n``` `\n\ntwo\n````\n\nafter",
		"\u00a0~~~\none\n\ntwo\n~~~\n\nafter",
	} {
		r := NewIncrementalRenderer(60)
		for end := range len(input) + 1 {
			if end < len(input) && !utf8.RuneStart(input[end]) {
				continue
			}
			got, blocks, err := r.RenderWithCodeBlocks(input[:end])
			require.NoError(t, err)
			want, wantBlocks, err := NewFastRenderer(60).RenderWithCodeBlocks(input[:end])
			require.NoError(t, err)
			require.Equal(t, want, got, "input %q at byte %d", input, end)
			require.Equal(t, wantBlocks, blocks, "input %q at byte %d", input, end)
		}
	}
}
