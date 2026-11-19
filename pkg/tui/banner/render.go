package banner

import (
	"image/color"
	"math"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

var brailleBits = [2][4]byte{{1, 2, 4, 64}, {8, 16, 32, 128}}

var sunsetRows = [Height]color.Color{
	lipgloss.Color("#B28AFF"),
	lipgloss.Color("#D981D5"),
	lipgloss.Color("#FF78AA"),
	lipgloss.Color("#FF8C95"),
	lipgloss.Color("#FFA080"),
	lipgloss.Color("#FFB46B"),
}

const (
	// The blank between the words separates their settling phases.
	agentStart = 43
	settledAt  = 0.90
)

// Render returns the static, theme-adjusted wordmark.
func Render() []string {
	colors := rowColors(styles.Background)
	lines := make([]string, Height)
	for y, line := range Lines {
		lines[y] = lipgloss.NewStyle().Width(Width).Foreground(colors[y]).Render(line)
	}
	return lines
}

// RenderFrame renders a pane-width particle frame. The settled hold uses Render exactly.
func RenderFrame(progress float64, paneWidth int) []string {
	if paneWidth < Width {
		return nil
	}
	if progress >= settledAt {
		return Render()
	}
	colors := rowColors(styles.Background)
	cells := make([][]byte, Height)
	for y := range cells {
		cells[y] = make([]byte, paneWidth)
	}
	for y, line := range Lines {
		for x, r := range []rune(line) {
			if r < '⠀' || r > '⣿' {
				continue
			}
			bits := byte(r - '⠀')
			for dx := range 2 {
				for dy := range 4 {
					if bits&brailleBits[dx][dy] == 0 {
						continue
					}
					px, py, _ := dustPosition(x*2+dx, y*4+dy, paneWidth, progress)
					if px < 0 || px >= paneWidth*2 || py < 0 || py >= Height*4 {
						continue
					}
					cells[py/4][px/2] |= brailleBits[px%2][py%4]
				}
			}
		}
	}
	lines := make([]string, Height)
	for y, row := range cells {
		var b strings.Builder
		for x := 0; x < len(row); {
			if row[x] == 0 {
				b.WriteByte(' ')
				x++
				continue
			}
			var run strings.Builder
			for x < len(row) && row[x] != 0 {
				run.WriteRune('⠀' + rune(row[x]))
				x++
			}
			b.WriteString(lipgloss.NewStyle().Foreground(colors[y]).Render(run.String()))
		}
		lines[y] = b.String()
	}
	return lines
}

func dustHash(x, y int) uint32 {
	h := uint32(x)*0x8da6b343 ^ uint32(y)*0xd8163841 //nolint:gosec // Coordinates are bounded, nonnegative mask positions.
	h ^= h >> 16
	h *= 0x7feb352d
	return h ^ h>>15
}

// dustPosition advances a narrow stream into a cloud that leaves settled pixels behind.
func dustPosition(x, y, paneWidth int, progress float64) (int, int, bool) {
	h := dustHash(x, y)
	target := (paneWidth-Width)/2*2 + x
	end := 0.28 + 0.60*float64(x)/float64(Width*2)
	if progress >= end {
		return target, y, true
	}
	start := float64(h%100) / 430
	if progress <= start {
		return -1, Height * 2, false
	}
	left := float64((paneWidth - Width) / 2 * 2)
	front := (left + float64(Width*2)) * progress / 0.88
	spread := min(1, progress/0.32)
	front = max(0, front)
	cloudX := front - float64((h>>8)%36)*spread
	cloudY := float64(Height*2) + 3*math.Sin(cloudX/8-progress*15) + float64(int((h>>16)%3)-1)*3*spread
	arrival := min(1, (progress-start)/0.12)
	cloudX = -2 + (cloudX+2)*arrival
	blend := max(0, min(1, (progress-(end-0.12))/0.12))
	blend = blend * blend * (3 - 2*blend)
	return int(math.Round(cloudX + (float64(target)-cloudX)*blend)),
		int(math.Round(cloudY + (float64(y)-cloudY)*blend)), false
}

func rowColors(bg color.Color) []color.Color {
	colors := make([]color.Color, Height)
	r, g, b := styles.ColorToRGB(bg)
	if 0.2126*r+0.7152*g+0.0722*b > 0.5 {
		grey := styles.EnsureContrast(lipgloss.Color("#383838"), bg)
		greyR, _, _ := styles.ColorToRGB(grey)
		// EnsureContrast can brighten against mid-tone backgrounds; keep the banner dark.
		if greyR > 56.0/255 {
			grey = lipgloss.Color("#151515")
		}
		for y := range colors {
			colors[y] = grey
		}
		return colors
	}
	for y, c := range sunsetRows {
		colors[y] = styles.EnsureContrast(c, bg)
	}
	return colors
}
