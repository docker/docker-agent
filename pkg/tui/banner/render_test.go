package banner

import (
	"image/color"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

// sunsetHex is the unadjusted gradient: violet, hot pink and apricot stops at rows 0, 2 and 5.
var sunsetHex = []string{"#b28aff", "#d981d5", "#ff78aa", "#ff8c95", "#ffa080", "#ffb46b"}

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

func TestRenderUsesSunsetGradientWithDefaultTheme(t *testing.T) {
	t.Parallel()

	lines := Render()
	require.Len(t, lines, Height)
	for y, rowHex := range sunsetHex {
		want := lipgloss.NewStyle().Width(Width).Foreground(lipgloss.Color(rowHex)).Render(Lines[y])
		assert.Equal(t, want, lines[y], "row %d", y)
	}
}

func TestRowColorsStayReadableOnThemes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		theme string
		light bool
	}{
		{theme: "default"},
		{theme: "solarized-dark"},
		{theme: "default-light", light: true},
		{theme: "gruvbox-light", light: true},
	}
	for _, tt := range tests {
		t.Run(tt.theme, func(t *testing.T) {
			t.Parallel()

			theme, err := styles.LoadTheme(tt.theme)
			require.NoError(t, err)
			bg := lipgloss.Color(theme.Colors.Background)
			colors := rowColors(bg)
			require.Len(t, colors, Height)

			got := make([]string, len(colors))
			for y, c := range colors {
				got[y] = hex(c)
				// EnsureContrast leaves a color unchanged once it meets the minimum contrast.
				assert.Equal(t, got[y], hex(styles.EnsureContrast(c, bg)), "row %d is readable", y)
			}
			if !tt.light {
				assert.Equal(t, sunsetHex, got, "dark themes show the gradient unchanged")
				return
			}
			for y := 1; y < len(got); y++ {
				assert.NotEqual(t, got[y-1], got[y], "rows %d and %d stay distinct", y-1, y)
			}
		})
	}
}

func hex(c color.Color) string {
	return styles.RGBToHex(styles.ColorToRGB(c))
}
