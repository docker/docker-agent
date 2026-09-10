package markdown

import (
	_ "embed"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIncrementalRenderedPartsPreserveMarkdownSemantics(t *testing.T) {
	t.Parallel()
	cases := map[string]string{"long heading": "# A long heading that wraps across multiple terminal rows while retaining heading styling and continuation indentation", "unordered list": "- first list item with enough words to wrap onto a continuation line that stays indented\n- second item", "ordered list": "1. first ordered item with enough words to wrap onto a continuation line that stays indented\n2. second item", "mixed blocks": "# Heading\n\nOpening **paragraph** with a [link](https://example.com).\n\n- list item\n  continued content\n\n1. ordered item\n2. next item\n\nFinal `code` paragraph."}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			for _, width := range []int{24, 47, 80} {
				r := NewIncrementalRenderer(width)
				for end := 1; end <= len(input); end++ {
					parts, err := r.RenderParts(input[:end])
					require.NoError(t, err)
					got := r.joinPrefixAndTail(parts.StablePrefix, parts.MutableTail)
					want, err := NewFastRenderer(width).Render(input[:end])
					require.NoError(t, err)
					require.Equal(t, want, got, "width %d byte %d", width, end)
				}
			}
		})
	}
}

func TestIncrementalRenderedPartsMatchOneShotAtEveryMarkdownBoundary(t *testing.T) {
	const input = "Thinking… λ界\n\n# Heading\n\nParagraph with **bold**, `more`, and [link](https://example.com).\n\n- one\n- two\n\n```console\nroot\nmore\n```\n\n## Result\n\nDone."
	for _, width := range []int{24, 47, 80} {
		t.Run(strconv.Itoa(width), func(t *testing.T) {
			r := NewIncrementalRenderer(width)
			for end := range len(input) + 1 {
				if end < len(input) && !utf8.RuneStart(input[end]) {
					continue
				}
				prefix := input[:end]
				parts, err := r.RenderParts(prefix)
				require.NoError(t, err)
				got := r.joinPrefixAndTail(parts.StablePrefix, parts.MutableTail)
				want, wantBlocks, err := NewFastRenderer(width).RenderWithCodeBlocks(prefix)
				require.NoError(t, err)
				require.Equal(t, want, got, "byte boundary %d", end)
				require.Equal(t, wantBlocks, parts.CodeBlocks, "code blocks at byte boundary %d", end)
			}
		})
	}
}

func TestIncrementalRenderedPartsMatchJoinedOutputAtEveryStep(t *testing.T) {
	t.Parallel()
	chunks := []string{"unfinished *em", "phasis* and [li", "nk](https://example.com)\n\n", "```go\nfmt.Print(\"λ界\")", "\n```\n\n- one", "\n- two\n\nfinal"}
	inc := NewIncrementalRenderer(64)
	var input strings.Builder
	for i, chunk := range chunks {
		input.WriteString(chunk)
		parts, err := inc.RenderParts(input.String())
		require.NoError(t, err)
		got := inc.joinPrefixAndTail(parts.StablePrefix, parts.MutableTail)
		expected, err := NewFastRenderer(64).Render(input.String())
		require.NoError(t, err)
		require.Equal(t, expected, got, "step %d", i)
	}
}

// final output matches a single full render. This is the correctness contract
// the optimization must preserve.
func TestIncrementalRendererScalingBound(t *testing.T) {
	const chunk = "Paragraph with **markdown**, Unicode λ界, and `code`.\n\n"
	for _, target := range []int{1 << 10, 10 << 10, 50 << 10, 100 << 10, 200 << 10} {
		r := NewIncrementalRenderer(100)
		var input strings.Builder
		for input.Len() < target {
			input.WriteString(chunk)
			parts, err := r.RenderParts(input.String())
			require.NoError(t, err)
			// Every completed paragraph advances the cached stable input boundary.
			// Therefore only the final incomplete chunk can remain subject to
			// reparse, independent of the accumulated transcript length.
			uncommitted := input.Len() - len(r.inputPrefix)
			assert.LessOrEqual(t, uncommitted, len(chunk))
			assert.Equal(t, r.outputPrefix, parts.StablePrefix)
		}
		assert.GreaterOrEqual(t, len(r.inputPrefix), input.Len()-len(chunk))
	}
}

func TestIncrementalRendererMatchesFullRender(t *testing.T) {
	t.Parallel()

	chunks := splitIntoStreamingChunks(streamingBenchmarkContent)

	full := NewFastRenderer(80)
	expected, err := full.Render(streamingBenchmarkContent)
	require.NoError(t, err)

	inc := NewIncrementalRenderer(80)
	var accumulated strings.Builder
	var got string
	for _, c := range chunks {
		accumulated.WriteString(c)
		got, err = inc.Render(accumulated.String())
		require.NoError(t, err)
	}

	require.Equal(t, expected, got, "incremental render diverged from full render")
}

// TestIncrementalRendererMatchesFullRenderForEachStep verifies that *every*
// intermediate output matches what a full render would produce for the same
// accumulated input. This catches per-chunk divergences (not just the final
// output).
func TestIncrementalRendererMatchesFullRenderForEachStep(t *testing.T) {
	t.Parallel()

	chunks := splitIntoStreamingChunks(streamingBenchmarkContent)

	full := NewFastRenderer(80)
	inc := NewIncrementalRenderer(80)
	var accumulated strings.Builder
	for i, c := range chunks {
		accumulated.WriteString(c)
		expected, err := full.Render(accumulated.String())
		require.NoError(t, err)
		got, err := inc.Render(accumulated.String())
		require.NoError(t, err)
		require.Equal(t, expected, got, "step %d (len=%d) diverged", i, accumulated.Len())
	}
}

// TestIncrementalRendererCorrectnessForVariousInputs runs the same equivalence
// check on a battery of small markdown samples that exercise different block
// types and tricky boundaries.
func TestIncrementalRendererCorrectnessForVariousInputs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"plain paragraph", "Hello there, this is a single paragraph."},
		{"two paragraphs", "First paragraph.\n\nSecond paragraph."},
		{"heading then paragraph", "# Title\n\nBody text follows."},
		{
			name:  "code fence then paragraph",
			input: "Intro.\n\n```go\nfunc main() {}\n```\n\nOutro.",
		},
		{
			name:  "open code fence at end",
			input: "Intro.\n\n```go\nfunc main() {",
		},
		{
			name:  "list followed by code",
			input: "- one\n- two\n\n```\ncode\n```",
		},
		{
			name:  "blank line inside fence",
			input: "```\nline 1\n\nline 3\n```\n\nafter",
		},
		{
			name:  "trailing blank line",
			input: "Paragraph one.\n\n",
		},
		{
			name:  "no trailing newline",
			input: "First paragraph.\n\nSecond without trailing newline",
		},
		{
			name:  "open fence no trailing newline",
			input: "Intro.\n\n```go\nfunc main",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			full := NewFastRenderer(80)
			expected, err := full.Render(tc.input)
			require.NoError(t, err)

			inc := NewIncrementalRenderer(80)
			got, err := inc.Render(tc.input)
			require.NoError(t, err)
			require.Equal(t, expected, got)

			// Also check chunked feeding (every byte) preserves correctness.
			inc2 := NewIncrementalRenderer(80)
			var acc strings.Builder
			for i := range len(tc.input) {
				acc.WriteByte(tc.input[i])
				_, err := inc2.Render(acc.String())
				require.NoError(t, err)
			}
			finalGot, err := inc2.Render(tc.input)
			require.NoError(t, err)
			require.Equal(t, expected, finalGot, "byte-by-byte streaming diverged from full render")
		})
	}
}

// TestIncrementalRendererHandlesWidthChanges checks that SetWidth invalidates
// the cache and subsequent renders match the full-render output at the new
// width.
func TestIncrementalRendererHandlesWidthChanges(t *testing.T) {
	t.Parallel()

	const sample = "# Title\n\nA paragraph that should wrap nicely at narrow widths.\n\n- one\n- two"

	inc := NewIncrementalRenderer(80)
	_, err := inc.Render(sample)
	require.NoError(t, err)

	inc.SetWidth(40)
	got, err := inc.Render(sample)
	require.NoError(t, err)

	full := NewFastRenderer(40)
	expected, err := full.Render(sample)
	require.NoError(t, err)

	require.Equal(t, expected, got)
}

// TestIncrementalRendererHandlesContentReplacement verifies that an unrelated
// new content (one that does not extend the cached prefix) triggers a full
// render and produces the correct output.
func TestIncrementalRendererHandlesContentReplacement(t *testing.T) {
	t.Parallel()

	inc := NewIncrementalRenderer(80)
	_, err := inc.Render("Hello there.\n\nMore text.\n\n")
	require.NoError(t, err)

	const next = "# Different\n\nCompletely unrelated content."
	got, err := inc.Render(next)
	require.NoError(t, err)

	full := NewFastRenderer(80)
	expected, err := full.Render(next)
	require.NoError(t, err)

	require.Equal(t, expected, got)
}

// BenchmarkStreamingIncrementalRenderer measures the streaming workload using
// the incremental renderer. Compare with BenchmarkStreamingFastRenderer (the
// full-render baseline) to see the speedup.
func BenchmarkStreamingIncrementalRenderer(b *testing.B) {
	chunks := splitIntoStreamingChunks(streamingBenchmarkContent)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		inc := NewIncrementalRenderer(80)
		var accumulated strings.Builder
		for _, c := range chunks {
			accumulated.WriteString(c)
			_, _ = inc.Render(accumulated.String())
		}
	}
}
