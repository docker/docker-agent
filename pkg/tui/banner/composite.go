package banner

import (
	"image"
	"math"
	"slices"

	uv "github.com/charmbracelet/ultraviolet"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

// Near reports whether the pointer is within the influence radius of a dot.
func (f *Flock) Near(x, y, progress float64, width int) bool {
	for _, p := range f.dots {
		px, py, _ := dustPosition(p.homeX, p.homeY, width, progress)
		if px >= 0 && math.Hypot(float64(px)+p.x-x*2, float64(py)+p.y-y*4) < influenceRadius {
			return true
		}
	}
	return false
}

// Composite overlays occupied braille cells only, clipping at the viewport and
// foreground masks. One screen buffer and one dot pass keep work linear.
func (f *Flock) Composite(base string, progress float64, paneWidth int, origin image.Point, viewport image.Rectangle, masks []image.Rectangle) string {
	if !f.Displaced() || viewport.Empty() {
		return base
	}
	screen := uv.NewScreenBuffer(viewport.Dx(), viewport.Dy())
	uv.NewStyledString(base).Draw(&screen, screen.Bounds())
	type ink struct {
		bits byte
		row  int
	}
	cells := make(map[image.Point]ink, len(f.dots))
	for _, p := range f.dots {
		x, y, _ := dustPosition(p.homeX, p.homeY, paneWidth, progress)
		if x < 0 {
			continue
		}
		x += int(math.Round(p.x)) + origin.X*2
		y += int(math.Round(p.y)) + origin.Y*4
		if x < 0 || y < 0 {
			continue
		}
		at := image.Pt(x/2, y/4)
		if !at.In(viewport) {
			continue
		}
		if slices.ContainsFunc(masks, at.In) {
			continue
		}
		c := cells[at]
		c.bits |= brailleBits[x%2][y%4]
		c.row = p.homeY / 4
		cells[at] = c
	}
	colors := rowColors(styles.Background)
	for at, c := range cells {
		under := screen.CellAt(at.X, at.Y)
		// Let particles pass behind text and controls rather than erase them.
		if under.Width != 1 || (under.Content != "" && under.Content != " ") {
			continue
		}
		cell := under.Clone()
		cell.Content = string('⠀' + rune(c.bits))
		cell.Width = 1
		cell.Style.Fg = colors[c.row]
		screen.SetCell(at.X, at.Y, cell)
	}
	return screen.Render()
}
