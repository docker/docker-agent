package tabbar

import (
	"image/color"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const (
	buttonHoverDuration = 150 * time.Millisecond
	buttonPulseDuration = 280 * time.Millisecond
)

type tabButton struct {
	sessionID string
	plus      bool
}

func (t *TabBar) hoverFraction(button tabButton) float64 {
	return t.buttonHover.Fraction(button)
}

func (t *TabBar) plusStyle() lipgloss.Style {
	pulse := 0.0
	if t.plusPulse.Running() {
		pulse = 1 - t.plusPulse.Value()
	}
	return buttonStyle(styles.Background, styles.MutedContrastFg(styles.Background), t.hoverFraction(tabButton{plus: true}), pulse)
}

func buttonStyle(bg, fg color.Color, hover, pulse float64) lipgloss.Style {
	// These are the composer's semantic palette equivalents of the
	// reference tab-hover and tab-press surfaces.
	if hover > 0 {
		bg = styles.BlendColor(bg, styles.BackgroundAlt, 0.3*hover)
		fg = styles.BlendColor(fg, styles.TextBright, 0.3*hover)
	}
	if pulse > 0 {
		bg = styles.BlendColor(bg, styles.Selected, 0.7*pulse)
		fg = styles.BlendColor(fg, styles.TextBright, 0.7*pulse)
	}
	return lipgloss.NewStyle().Background(bg).Foreground(fg)
}

// UpdateHover handles tab-local pointer cells; a negative x leaves the strip.
func (t *TabBar) UpdateHover(x int) tea.Cmd {
	var target tabButton
	if !t.drag.active {
		for _, z := range t.zones {
			if x >= z.startX && x < z.endX {
				switch {
				case z.isPlus:
					target.plus = true
				case z.isClose && z.tabIdx >= 0 && z.tabIdx < len(t.tabs):
					target.sessionID = t.tabs[z.tabIdx].SessionID
				}
				break
			}
		}
	}
	if target == t.hoveredButton {
		return nil
	}
	t.hoveredButton = target
	return t.buttonHover.Set(target)
}

// TickHover advances button fades on the owner's clock.
func (t *TabBar) TickHover(tick animation.TickMsg) {
	t.buttonHover.Tick(tick)
	if t.plusPulse.Running() {
		t.plusPulse.Tick()
		tick.MarkDirty()
	}
}

// ClearHover immediately resets pointer hover fades while letting an in-flight
// click pulse finish.
func (t *TabBar) ClearHover() {
	if t.buttonHover.Clear() {
		t.visualGeneration++
	}
	t.hoveredButton = tabButton{}
}

// CancelHover immediately resets all hover and click-pulse feedback when the
// strip loses visibility or focus.
func (t *TabBar) CancelHover() {
	t.ClearHover()
	if t.plusPulse.Running() {
		t.visualGeneration++
		t.plusPulse.Cancel()
	}
}
