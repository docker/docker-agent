package latex

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gotest.tools/v3/golden"
)

func TestRender(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		source  string
		display bool
	}{
		{"blackboard and scripts", `\mathbb{C}^3 \to \mathbb{C}^3`, false},
		{"fraction", `F_1 = -\frac{1}{4x^2}`, false},
		{"operators", `\sum_{i=0}^n \alpha_i + \int_0^\infty e^{-x^2}\,dx`, false},
		{"root and relation", `x=\frac{-b\pm\sqrt{b^2-4ac}}{2a}`, false},
		{"matrix", `\begin{pmatrix}1&200\\3000&4\end{pmatrix}`, false},
		{"matrix equation", `A =
\begin{pmatrix}
1 & 2 \\
3 & 4
\end{pmatrix},
\qquad
\det(A) = -2`, true},
		{"display fraction", `\frac{0.1\ \text{lux}}{100\ \text{lm/W}}`, true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			rendered, ok := Render(test.source, test.display)
			require.True(t, ok)
			golden.Assert(t, rendered, test.name+".golden")
		})
	}
}

func TestRenderRejectsUnsupportedAndMalformed(t *testing.T) {
	t.Parallel()

	for _, source := range []string{`x + \unknown{y}`, `\frac{1}`, `{x`} {
		_, ok := Render(source, false)
		assert.False(t, ok, source)
	}
}
