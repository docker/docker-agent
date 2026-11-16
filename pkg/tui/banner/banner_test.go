package banner

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
)

func TestBannerDimensions(t *testing.T) {
	t.Parallel()

	assert.Len(t, Lines, Height)
	width := 0
	for _, line := range Lines {
		width = max(width, ansi.StringWidth(line))
		assert.Equal(t, line, ansi.Strip(line), "the caller supplies theme colors")
		assert.Equal(t, strings.TrimRight(line, " "), line)
		for _, r := range line {
			assert.True(t, r == ' ' || (r >= '\u2801' && r <= '\u28ff'), "only braille dots and spaces, got %q", r)
		}
	}
	assert.Equal(t, Width, width)
}
