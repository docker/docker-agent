package banner

import (
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

// Render applies a top-to-bottom accent-to-info gradient using the current theme.
func Render() []string {
	start := styles.EnsureContrast(styles.Accent, styles.Background)
	end := styles.EnsureContrast(styles.Info, styles.Background)
	r0, g0, b0 := styles.ColorToRGB(start)
	r1, g1, b1 := styles.ColorToRGB(end)

	lines := make([]string, len(Lines))
	for y, line := range Lines {
		t := float64(y) / float64(Height-1)
		fg := styles.RGBToColor(
			r0+(r1-r0)*t,
			g0+(g1-g0)*t,
			b0+(b1-b0)*t,
		)
		// Equal-width rows keep centering from shifting the g's descender.
		lines[y] = lipgloss.NewStyle().Width(Width).Foreground(fg).Render(line)
	}
	return lines
}
