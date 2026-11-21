package scrollview

import (
	"sync/atomic"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

const (
	fadeHeightPercent = 0.15
	fadeMinOpacity    = 0.50
	maxFadeLines      = 20
)

var edgeFadeDisabled atomic.Bool

// SetEdgeFadeEnabled controls edge fades for existing and future scrollviews.
func SetEdgeFadeEnabled(enabled bool) {
	edgeFadeDisabled.Store(!enabled)
}

type fadedLine struct {
	input, output string
	opacity       float64
}

type edgeFade struct {
	palette     styles.FadePalette
	top, bottom [maxFadeLines]fadedLine
}

func (m *Model) fadeEdges(lineCount int) (top, bottom int) {
	if m.contentHeight <= 0 {
		return 0, 0
	}
	n := min(max(1, int(float64(m.height)*fadeHeightPercent)), maxFadeLines, lineCount/2)
	if n == 0 {
		return 0, 0
	}
	if m.scrollOffset > 0 {
		top = n
	}
	if m.scrollOffset+m.height < m.contentHeight {
		bottom = n
	}
	if top != 0 || bottom != 0 {
		palette := styles.CurrentFadePalette()
		if m.fade == nil || m.fade.palette != palette {
			m.fade = &edgeFade{palette: palette}
		}
	}
	return top, bottom
}

func (f *edgeFade) line(line string, count, row, top, bottom int) string {
	switch {
	case row < top:
		return f.cachedLine(line, &f.top[row], row, top)
	case count-1-row < bottom:
		i := count - 1 - row
		return f.cachedLine(line, &f.bottom[i], i, bottom)
	default:
		return line
	}
}

func (f *edgeFade) cachedLine(line string, entry *fadedLine, i, n int) string {
	opacity := fadeMinOpacity + (1-fadeMinOpacity)*float64(i+1)/float64(n+1)
	if entry.input != line || entry.opacity != opacity {
		*entry = fadedLine{input: line, opacity: opacity, output: f.palette.FadeLine(line, opacity)}
	}
	return entry.output
}
