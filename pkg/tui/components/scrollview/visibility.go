package scrollview

import (
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const (
	scrollbarHold = 2 * animation.MediumDuration
	scrollbarFade = 150 * time.Millisecond
)

type scrollbarVisibility struct {
	clock     *animation.Runtime
	ticks     animation.Subscription
	alpha     float64
	holdUntil time.Duration
	x, y      int
}

// SetAnimationRuntime binds scrollbar animation to the owning program's clock.
// Without a clock, hover and capture are painted immediately.
func (m *Model) SetAnimationRuntime(clock *animation.Runtime) {
	if m.visibility.clock == clock {
		return
	}
	m.ClearScrollbarPointer()
	m.visibility.clock = clock
	if clock != nil {
		m.visibility.ticks = clock.Subscribe()
	}
}

// RevealScrollbar is called by owners when user input moves their viewport.
func (m *Model) RevealScrollbar() tea.Cmd {
	v := &m.visibility
	if v.clock == nil || !m.NeedsScrollbar() || m.height <= 0 {
		return nil
	}
	v.holdUntil = v.clock.Now() + scrollbarHold
	return m.syncScrollbarAnimation()
}

// SetScrollbarPointer observes motion without taking mouse capture. Negative
// coordinates clear hover, for example when another surface takes focus.
func (m *Model) SetScrollbarPointer(x, y int) tea.Cmd {
	wasHovered := m.pointerOverScrollbar()
	v := &m.visibility
	v.x, v.y = x, y
	if v.clock != nil && !wasHovered && m.pointerOverScrollbar() {
		v.holdUntil = v.clock.Now() + scrollbarHold
	}
	return m.syncScrollbarAnimation()
}

func (m *Model) pointerOverScrollbar() bool {
	const proximity = 2
	v := &m.visibility
	return m.NeedsScrollbar() && v.x >= 0 && v.y >= 0 &&
		v.x >= max(m.xPos, m.ScrollbarX()-proximity) && v.x <= m.ScrollbarX()+proximity &&
		v.y >= m.yPos && v.y < m.yPos+m.height
}

// reconcileScrollbarVisibility handles geometry and content changes, which
// have no command to return. A view without a scrollbar drops presentation,
// tick lease, and capture at once, so restored overflow never resumes stale
// opacity or a drag. Otherwise an active tick chain reconciles on its next
// frame; a settled bar that no longer sits under the pointer has no chain to
// fade on, so it is hidden rather than left painted until the next mouse event.
func (m *Model) reconcileScrollbarVisibility() {
	v := &m.visibility
	if !m.NeedsScrollbar() || m.height <= 0 {
		m.resetScrollbarPresentation()
		return
	}
	if v.ticks.IsActive() || v.alpha == 0 || m.IsDragging() || m.pointerOverScrollbar() {
		return
	}
	v.alpha, v.holdUntil = 0, 0
}

func (m *Model) syncScrollbarAnimation() tea.Cmd {
	v := &m.visibility
	if v.clock == nil || !m.NeedsScrollbar() || m.height <= 0 {
		return nil
	}
	wantsVisible := m.pointerOverScrollbar() || m.IsDragging()
	if (wantsVisible && v.alpha < 1) || (!wantsVisible && v.alpha > 0) || v.clock.Now() < v.holdUntil {
		return v.ticks.Start()
	}
	v.ticks.Stop()
	return nil
}

// UpdateScrollbarAnimation paints a new opacity only on accepted shared ticks.
func (m *Model) UpdateScrollbarAnimation(tick animation.TickMsg) {
	v := &m.visibility
	if !v.ticks.IsActive() {
		return
	}
	if !m.NeedsScrollbar() || m.height <= 0 {
		m.ClearScrollbarPointer()
		return
	}
	before, after := tick.ElapsedBounds()
	old := v.alpha
	if m.pointerOverScrollbar() || m.IsDragging() {
		v.alpha = min(1, v.alpha+float64(after-before)/float64(scrollbarFade))
	} else {
		// A delayed frame can cross the dwell deadline. Spend only the time
		// after that deadline fading out, not the entire time since the last tick.
		endOfHold := min(after, max(before, v.holdUntil))
		v.alpha = min(1, v.alpha+float64(endOfHold-before)/float64(scrollbarFade))
		v.alpha = max(0, v.alpha-float64(after-endOfHold)/float64(scrollbarFade))
	}
	if old != v.alpha {
		tick.MarkDirty()
	}
	if after >= v.holdUntil && ((v.alpha == 1 && (m.pointerOverScrollbar() || m.IsDragging())) || v.alpha == 0) {
		v.ticks.Stop()
	}
}

// SetPositionAndReconcileScrollbar places a viewport during rendering and
// reports whether its scrollbar must be repainted. With no tick chain to
// animate a layout change, settled opacity follows the final pointer geometry.
func (m *Model) SetPositionAndReconcileScrollbar(x, y int) bool {
	before := m.scrollbarOpacity()
	m.SetPosition(x, y)
	v := &m.visibility
	if v.clock != nil && !v.ticks.IsActive() && m.pointerOverScrollbar() {
		v.alpha = 1
	}
	return m.scrollbarOpacity() != before
}

func (m *Model) scrollbarOpacity() float64 {
	if m.visibility.clock == nil {
		if m.pointerOverScrollbar() || m.IsDragging() {
			return 1
		}
		return 0
	}
	return m.visibility.alpha
}

func (m *Model) scrollbarLines() []string {
	alpha := m.scrollbarOpacity()
	if alpha <= 0 {
		return nil
	}
	lines := m.sb.ViewLines()
	if alpha >= 1 {
		return lines
	}
	// A scrollbar has at most two styles per frame (track and thumb).
	palette := styles.CurrentFadePalette()
	paint := make(map[string]string, 2)
	out := make([]string, len(lines))
	for i, line := range lines {
		faded, ok := paint[line]
		if !ok {
			faded = palette.FadeLine(line, alpha)
			paint[line] = faded
		}
		out[i] = faded
	}
	return out
}

// ClearScrollbarPointer resets presentation and releases capture on a surface
// that is no longer receiving input.
func (m *Model) ClearScrollbarPointer() {
	m.visibility.x, m.visibility.y = -1, -1
	m.resetScrollbarPresentation()
}

// resetScrollbarPresentation hides the bar, releases its tick lease, and ends
// any capture while keeping the last observed pointer.
func (m *Model) resetScrollbarPresentation() {
	v := &m.visibility
	v.alpha, v.holdUntil = 0, 0
	v.ticks.Stop()
	if m.IsDragging() {
		_, _ = m.sb.Update(tea.MouseReleaseMsg{Button: tea.MouseLeft})
	}
}

func (m *Model) onScrollbar(x, y int) bool {
	return m.NeedsScrollbar() && x == m.ScrollbarX() && y >= m.yPos && y < m.yPos+m.height
}
