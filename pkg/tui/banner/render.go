package banner

import (
	"image/color"

	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

// sunsetRows has violet, hot pink and apricot stops at rows 0, 2 and 5; the other rows blend their neighbors.
var sunsetRows = [Height]color.Color{
	lipgloss.Color("#B28AFF"),
	lipgloss.Color("#D981D5"),
	lipgloss.Color("#FF78AA"),
	lipgloss.Color("#FF8C95"),
	lipgloss.Color("#FFA080"),
	lipgloss.Color("#FFB46B"),
}

// Render applies the top-to-bottom sunset gradient, readable on the current theme background.
func Render() []string {
	colors := rowColors(styles.Background)
	lines := make([]string, len(Lines))
	for y, line := range Lines {
		// Equal-width rows keep centering from shifting the g's descender.
		lines[y] = lipgloss.NewStyle().Width(Width).Foreground(colors[y]).Render(line)
	}
	return lines
}

// rowColors adjusts only the rows that are hard to read on bg, such as on light themes.
func rowColors(bg color.Color) []color.Color {
	colors := make([]color.Color, len(sunsetRows))
	for y, c := range sunsetRows {
		colors[y] = styles.EnsureContrast(c, bg)
	}
	return colors
}
