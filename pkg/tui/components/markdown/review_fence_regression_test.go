package markdown

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

func TestListMarkerWhitespacePreservesFenceAndFollowingProse(t *testing.T) {
	for _, input := range []string{
		"-   step\n    ~~~\n    echo hi\n      ~~~\nafter",
		"12.   step\n      ~~~\n      echo hi\n        ~~~\nafter",
		"  - \tstep\n    ~~~\n    echo hi\n      ~~~\nafter",
	} {
		view, blocks, err := NewFastRenderer(60).RenderWithSourceCodeBlocks(input)
		require.NoError(t, err)
		require.Len(t, blocks, 1, input)
		require.Equal(t, "echo hi", blocks[0].Content, input)
		require.Contains(t, ansi.Strip(view), "after", input)
	}
}

func TestFenceStreamingReviewRegressions(t *testing.T) {
	for _, input := range []string{
		"-   step\n    ~~~\n    echo hi\n      ~~~\nafter\n\nend",
		"- item\n  paragraph\n  ~~~\nfirst\n    ~~~\nhello\n\nsecond\n  ~~~\n\nafter",
		"- item\n  > quote\n  > next\n  ~~~\n  first\n      ~~~\n\n  second\n  ~~~\n\nafter",
		"- item\n\n~~~\nx\n~~~\n\nafter",
		"- item\n\n\n  ~~~\n  x\n    ~~~\n\nmid\n\nend",
		"- item\n\n  \n  ~~~\n  x\n    ~~~\n\nmid\n\n~~~\ncode1\n\ncode2\n~~~\n\nend",
		"- item\n\n\t\n  ~~~\n  x\n    ~~~\n\nmid\n\n~~~\ncode1\n\ncode2\n~~~\n\nend",
		"intro\n\n```go\n```\n\nafter",
		"~~~\n\n~~~\n\nafter",
		"~~~\nx\n~~~\n\nexplanation\n\n~~~\ny\n~~~\n\nend",
	} {
		for _, width := range []int{24, 60} {
			oneShot, oneShotBlocks, err := NewIncrementalRenderer(width).RenderWithCodeBlocks(input)
			require.NoError(t, err)
			full, fullBlocks, err := NewFastRenderer(width).RenderWithCodeBlocks(input)
			require.NoError(t, err)
			require.Equal(t, full, oneShot, "one-shot input %q width %d", input, width)
			require.Equal(t, fullBlocks, oneShotBlocks, "one-shot blocks input %q width %d", input, width)
			r := NewIncrementalRenderer(width)
			for end := range len(input) + 1 {
				got, blocks, err := r.RenderWithCodeBlocks(input[:end])
				require.NoError(t, err)
				want, wantBlocks, err := NewFastRenderer(width).RenderWithCodeBlocks(input[:end])
				require.NoError(t, err)
				require.Equal(t, want, got, "input %q width %d byte %d", input, width, end)
				require.Equal(t, wantBlocks, blocks, "input %q width %d byte %d", input, width, end)
			}
		}
	}
}

func TestTopLevelFenceStreamingAdvancesPrefix(t *testing.T) {
	const chunk = "~~~\nx\n~~~\n\nexplanation\n\n"
	r := NewIncrementalRenderer(60)
	var input strings.Builder
	for range 30 {
		input.WriteString(chunk)
		_, err := r.RenderParts(input.String())
		require.NoError(t, err)
		require.LessOrEqual(t, input.Len()-len(r.inputPrefix), len(chunk))
	}
}
