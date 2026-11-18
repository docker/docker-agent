package editfile

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWrapTokensInvalidUTF8 pins that width wrapping counts source bytes, not
// utf8.RuneLen's 3-byte U+FFFD replacement: an invalid byte must not slice
// past the token text (the diff renderer panics otherwise on Latin-1/binary
// content).
func TestWrapTokensInvalidUTF8(t *testing.T) {
	t.Parallel()

	lines := wrapTokens([]chromaToken{{Text: "a\xff"}}, 10)
	require.NotEmpty(t, lines)

	var rebuilt strings.Builder
	for _, line := range lines {
		for _, tok := range line {
			rebuilt.WriteString(tok.Text)
		}
	}
	assert.Equal(t, "a\xff", rebuilt.String(), "wrapping must keep every source byte")
}
