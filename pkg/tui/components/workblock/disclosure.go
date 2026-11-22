package workblock

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/animation"
)

// disclosure is the reveal motion of a Work body. It owns only the animation
// state; the inter-agent surface and header live with the agentmessage package.
type disclosure struct {
	motion        animation.Transition
	expanded      bool
	userSet       bool // an explicit user toggle outranks any default
	from, visible float64
}

func newDisclosure(ar *animation.Runtime) disclosure {
	return disclosure{motion: ar.Transition()}
}

func (d *disclosure) Expanded() bool  { return d.expanded }
func (d *disclosure) NeedsTick() bool { return d.motion.Running() }
func (d *disclosure) Visible() bool   { return d.visible > 0 }

// Toggle returns the lease start for the reveal; the caller must surface it.
func (d *disclosure) Toggle() tea.Cmd {
	d.userSet = true
	d.from = d.visible
	d.expanded = !d.expanded
	return d.motion.Start(140*time.Millisecond, animation.EaseOutCubic)
}

// SetDefault opens or closes the disclosure without animation until the user
// toggles it. It reports whether the visible state changed.
func (d *disclosure) SetDefault(expanded bool) bool {
	if d.userSet || d.expanded == expanded {
		return false
	}
	d.expanded = expanded
	d.Settle()
	return true
}

func (d *disclosure) Tick(tick animation.TickMsg) {
	if !d.motion.Running() {
		return
	}
	d.motion.Tick()
	target := 0.0
	if d.expanded {
		target = 1
	}
	d.visible = d.from + (target-d.from)*d.motion.Value()
	tick.MarkDirty()
}

// Settle releases the lease when hidden, removed or resized.
func (d *disclosure) Settle() {
	d.motion.Cancel()
	d.visible = 0
	if d.expanded {
		d.visible = 1
	}
}

// Reveal returns the portion of a disclosure body visible at the current motion frame.
func (d *disclosure) Reveal(body string) string {
	if d.visible <= 0 || body == "" {
		return ""
	}
	lines := strings.Split(body, "\n")
	rows := min(len(lines), int(float64(len(lines))*d.visible+0.5))
	if rows == 0 {
		return ""
	}
	return strings.Join(lines[:rows], "\n")
}
