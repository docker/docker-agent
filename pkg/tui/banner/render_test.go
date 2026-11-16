package banner

import (
	"strconv"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestRenderPreservesArt(t *testing.T) {
	t.Parallel()

	lines := Render()
	require.Len(t, lines, Height)
	for i, line := range lines {
		assert.Equal(t, Lines[i], strings.TrimRight(ansi.Strip(line), " "))
		assert.Equal(t, Width, ansi.StringWidth(line))
	}
	assert.NotEqual(t, Lines, lines, "the rendered banner includes color")
}

func TestRenderKeepsRowsAlignedWhenCentered(t *testing.T) {
	t.Parallel()

	const viewportWidth = Width + 20
	centered := lipgloss.PlaceHorizontal(viewportWidth, lipgloss.Center, strings.Join(Render(), "\n"))
	lines := strings.Split(ansi.Strip(centered), "\n")
	require.Len(t, lines, Height)
	for y, line := range lines {
		assert.Equal(t, strings.Repeat(" ", 10)+Lines[y], strings.TrimRight(line, " "))
	}
}

func TestRenderUsesThemeGradient(t *testing.T) {
	t.Parallel()

	start := styles.EnsureContrast(styles.Accent, styles.Background)
	end := styles.EnsureContrast(styles.Info, styles.Background)
	r0, g0, b0 := styles.ColorToRGB(start)
	r1, g1, b1 := styles.ColorToRGB(end)
	lines := Render()
	for y, line := range Lines {
		t.Run(strconv.Itoa(y), func(t *testing.T) {
			fraction := float64(y) / float64(Height-1)
			fg := styles.RGBToColor(r0+(r1-r0)*fraction, g0+(g1-g0)*fraction, b0+(b1-b0)*fraction)
			assert.Equal(t, lipgloss.NewStyle().Width(Width).Foreground(fg).Render(line), lines[y])
		})
	}
}
