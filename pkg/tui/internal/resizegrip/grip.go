// Package resizegrip implements the visual feedback shared by the two TUI resize handles.
package resizegrip

import (
	"image/color"
	"math"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const fadeDuration = 150 * time.Millisecond

// Reveal fades edge hover and direct handle hover independently. It holds one
// subscription, only while either value is changing. The zero value snaps to
// its targets, for models rendered without an animation runtime.
type Reveal struct {
	sub                      animation.Subscription
	animated                 bool
	edge, handle             float64
	edgeTarget, handleTarget float64
	dragging                 bool
	generation               uint64
}

func NewReveal(ar *animation.Runtime) Reveal {
	return Reveal{sub: ar.Subscribe(), animated: true}
}

// Generation lets the pointer fast path detect immediate visual changes.
func (r *Reveal) Generation() uint64 { return r.generation }

func (r *Reveal) Set(edge, handle, dragging bool) tea.Cmd {
	r.edgeTarget, r.handleTarget = 0, 0
	if edge {
		r.edgeTarget = 1
	}
	if edge && handle {
		r.handleTarget = 1
	}
	if r.dragging != dragging {
		r.dragging = dragging
		r.generation++
	}
	// Start the release fade at the edge's revealed color, not at a
	// stale direct-hover color from before capture.
	if dragging {
		r.edgeTarget, r.handleTarget = 1, 0
	}
	if dragging || !r.animated {
		if r.edge != r.edgeTarget || r.handle != r.handleTarget {
			r.edge, r.handle = r.edgeTarget, r.handleTarget
			r.generation++
		}
		r.sub.Stop()
		return nil
	}
	if r.edge == r.edgeTarget && r.handle == r.handleTarget {
		r.sub.Stop()
		return nil
	}
	return r.sub.Start()
}

func (r *Reveal) Reset() {
	if r.edge != 0 || r.handle != 0 || r.dragging {
		r.generation++
	}
	r.edge, r.handle, r.edgeTarget, r.handleTarget = 0, 0, 0, 0
	r.dragging = false
	r.sub.Stop()
}

func (r *Reveal) Tick(tick animation.TickMsg) {
	if !r.sub.IsActive() {
		return
	}
	before, after := tick.ElapsedBounds()
	delta := float64(after-before) / float64(fadeDuration)
	edge, handle := step(r.edge, r.edgeTarget, delta), step(r.handle, r.handleTarget, delta)
	if edge != r.edge || handle != r.handle {
		r.edge, r.handle = edge, handle
		r.generation++
		tick.MarkDirty()
	}
	if edge == r.edgeTarget && handle == r.handleTarget {
		r.sub.Stop()
	}
}

func step(value, target, delta float64) float64 {
	if math.Abs(target-value) <= delta+1e-12 {
		return target
	}
	return value + math.Copysign(delta, target-value)
}

// Render draws count copies of a one-cell grip glyph. At rest it emits only
// unstyled spaces, so no line is visible even on a different terminal background.
func (r *Reveal) Render(glyph string, count int) string {
	if !r.dragging && r.edge == 0 {
		return strings.Repeat(" ", count)
	}
	segment := strings.Repeat(glyph, count)
	switch {
	case r.dragging:
		return darkenedStyle().Render(segment)
	case r.handle == 1 && r.edge == 1:
		return styles.ResizeHandleHoverStyle.Render(segment)
	default:
		fg := blend(styles.ResizeHandleStyle.GetForeground(), styles.ResizeHandleHoverStyle.GetForeground(), r.handle)
		return styles.ResizeHandleStyle.Foreground(blend(styles.Background, fg, r.edge)).Render(segment)
	}
}

func darkenedStyle() lipgloss.Style {
	r, g, b := styles.ColorToRGB(styles.ResizeHandleStyle.GetForeground())
	return styles.ResizeHandleStyle.Foreground(styles.RGBToColor(r*0.7, g*0.7, b*0.7))
}

// RenderGuide fades the sidebar guide against the terminal background.
func RenderGuide(opacity float64) string {
	style := styles.ResizeHandleActiveStyle.Bold(false)
	return style.Foreground(blend(styles.Background, style.GetForeground(), opacity)).Render("│")
}

func blend(from, to color.Color, amount float64) color.Color {
	if amount == 0 {
		return from
	}
	if amount == 1 {
		return to
	}
	r, g, b := styles.ColorToRGB(from)
	tr, tg, tb := styles.ColorToRGB(to)
	return styles.RGBToColor(r+(tr-r)*amount, g+(tg-g)*amount, b+(tb-b)*amount)
}

// Segment gives the centered compact grip range within an existing hit area.
// The sidebar passes only the space below its toggle glyph.
func Segment(size, length int) (start, end int) {
	length = min(max(size, 0), length)
	start = (max(size, 0) - length) / 2
	return start, start + length
}
