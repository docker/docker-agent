package notification

import (
	"image"
	"slices"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/animation"
)

const avoidanceDuration = 350 * time.Millisecond

type avoidance struct {
	runtime            *animation.Runtime
	motion             animation.Transition
	occupied           []image.Rectangle
	layoutRevision     uint64
	lift, from, target int
	clipBottom         int
}

// SetAvoidance moves the stack clear of occupied input cells in screen
// coordinates. Empty input restores the ordinary bottom-right placement.
func (n *Manager) SetAvoidance(ar *animation.Runtime, cells []image.Rectangle) tea.Cmd {
	bounds := n.itemBounds()
	if len(bounds) == 0 {
		n.ResetAvoidance()
		return nil
	}
	a := &n.avoidance
	if a.runtime == ar && a.layoutRevision == n.layout.revision && slices.Equal(a.occupied, cells) {
		return nil
	}
	if a.runtime != ar {
		a.motion.Cancel()
		a.runtime = ar
		if ar != nil {
			a.motion = ar.Transition()
		}
		a.target = a.lift
	}
	a.layoutRevision = n.layout.revision
	a.occupied = append(a.occupied[:0], cells...)
	target, clipBottom := n.avoidanceTarget(bounds, cells)
	clipChanged := a.clipBottom != clipBottom
	a.clipBottom = clipBottom
	// Resizing can clip a still-moving stack against the top of the screen.
	// Retarget from its displayed position, not the pre-resize lift.
	if target == a.target && a.lift <= n.layout.top {
		if clipChanged {
			return n.refreshAvoidanceHover()
		}
		return nil
	}
	a.lift = min(a.lift, n.layout.top)
	a.from, a.target = a.lift, target
	if ar == nil || a.from == target {
		a.motion.Cancel()
		a.lift = target
		return n.refreshAvoidanceHover()
	}
	return tea.Batch(a.motion.Start(avoidanceDuration, animation.EaseOutCubic), n.refreshAvoidanceHover())
}

// ResetAvoidance releases movement work when the host stops displaying the stack.
func (n *Manager) ResetAvoidance() {
	n.avoidance.motion.Cancel()
	n.avoidance = avoidance{}
}

func (n *Manager) avoidanceTarget(bounds []notifBounds, cells []image.Rectangle) (lift, clipBottom int) {
	if len(cells) == 0 {
		return 0, n.height
	}
	// Solve from the resting stack; using its current position would reverse
	// the animation as soon as an intermediate frame cleared the text.
	limit := n.layout.top
	screen := image.Rect(0, 0, n.width, n.height)
	for {
		next, bottom := lift, n.height
		for _, b := range bounds {
			row := b.row + n.layout.lift
			rect := image.Rect(b.col, row-lift, b.col+b.width, row+b.height-lift)
			for _, cell := range cells {
				cell = cell.Intersect(screen)
				if rect.Overlaps(cell) {
					next = max(next, row+b.height-cell.Min.Y)
					bottom = min(bottom, cell.Min.Y)
				}
			}
		}
		// At the top of the screen there is nowhere left to move.
		// Keep only the rows above the first remaining overlap.
		if lift == limit || next == lift {
			return lift, bottom
		}
		lift = min(next, limit)
	}
}

func (n *Manager) tickAvoidance(msg animation.TickMsg) tea.Cmd {
	a := &n.avoidance
	if !a.motion.Running() {
		return nil
	}
	a.motion.Tick()
	lift := a.motion.Lerp(a.from, a.target)
	if lift == a.lift {
		return nil
	}
	a.lift = lift
	msg.MarkDirty()
	return n.refreshAvoidanceHover()
}

func (n *Manager) refreshAvoidanceHover() tea.Cmd {
	if n.mouseKnown {
		// The pointer can enter or leave a notification without moving itself.
		_, cmd := n.HandleMouseMotion(n.mouseX, n.mouseY)
		return cmd
	}
	return nil
}

// clipOverflow applies only when the stack has used all of its upward room.
func (n *Manager) clipOverflow() (int, bool) {
	a := &n.avoidance
	return a.clipBottom, a.lift == a.target && len(a.occupied) > 0 && a.clipBottom < n.height
}
