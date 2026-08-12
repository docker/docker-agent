package markdown

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLatexInline(t *testing.T) {
	t.Parallel()

	input := `A map $\mathbb{C}^3 \to \mathbb{C}^3$, $xy$, $x-y$, $-x$, $\frac{1}{2}$, and \(s \to \infty\).`
	result, err := NewFastRenderer(80).Render(input)
	require.NoError(t, err)
	assert.Equal(t, "A map ℂ³ → ℂ³, xy, x-y, -x, 1/2, and s → ∞.", strings.TrimRight(stripANSI(result), " "))
}

func TestLatexDisplayDelimiters(t *testing.T) {
	t.Parallel()

	input := `Before

\[
E \approx \frac{0.1\ \text{lux}}{100\ \text{lm/W}}
\]

after`
	result, err := NewFastRenderer(80).Render(input)
	require.NoError(t, err)
	assert.Equal(t, "Before\n\nE ≈ 0.1 lux\n────────\n100 lm/W\n\nafter", trimLinePadding(stripANSI(result)))

	dollars, err := NewFastRenderer(80).Render("$$\\{3x+2y,\\; x \\in \\{0, \\pm 1\\}\\}$$")
	require.NoError(t, err)
	assert.Equal(t, "{3x+2y, x ∈ {0, ± 1}}", strings.TrimRight(stripANSI(dollars), " "))
}

func TestLatexInsideMarkdownStructures(t *testing.T) {
	t.Parallel()

	input := "- Formula: $F_1 = u^2$\n\n| Value |\n| --- |\n| $\\mathbb{C}^3$ |"
	result, err := NewFastRenderer(80).Render(input)
	require.NoError(t, err)
	plain := stripANSI(result)
	assert.Contains(t, plain, "Formula: F₁ = u²")
	assert.Contains(t, plain, "ℂ³")
}

func TestLatexPreservesNonMathAndUnsupportedInput(t *testing.T) {
	t.Parallel()

	cases := []string{
		"Costs $5 and $10 or $8k–$12k; use `$x$`, $HOME, and ${PATH}.",
		`Unknown $x + \unknown{y}$ after`,
		`Streaming $\mathbb{C}^3`,
		`Escaped \$x-y\$.`,
	}
	for _, input := range cases {
		result, err := NewFastRenderer(100).Render(input)
		require.NoError(t, err)
		expected := strings.ReplaceAll(input, `\$`, `$`)
		expected = strings.ReplaceAll(expected, "`", "")
		assert.Equal(t, expected, strings.TrimRight(stripANSI(result), " "), input)
	}
}

func trimLinePadding(value string) string {
	lines := strings.Split(value, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ")
	}
	return strings.Join(lines, "\n")
}

func TestLatexUnclosedDelimitersKeepRenderingMarkdown(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input    string
		expected string
	}{
		{`See \( x more examples and **note** this`, "See ( x more examples and note this"},
		{`See \[ x more examples and **note** this`, "See [ x more examples and note this"},
	}
	for _, test := range tests {
		result, err := NewFastRenderer(100).Render(test.input)
		require.NoError(t, err)
		assert.Contains(t, stripANSI(result), test.expected)
		assert.Contains(t, result, "\x1b[1m", "bold Markdown after an unclosed delimiter should still render")
	}
}

func TestLatexDoesNotRenderInsideCode(t *testing.T) {
	t.Parallel()

	input := "`$x^2$`\n\n```text\n$\\mathbb{C}^3$\n```"
	result, err := NewFastRenderer(80).HideCopyIcon().Render(input)
	require.NoError(t, err)
	plain := stripANSI(result)
	assert.Contains(t, plain, "$x^2$")
	assert.Contains(t, plain, "$\\mathbb{C}^3$")
}
