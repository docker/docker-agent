package chat

import (
	"math"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/internal/resizegrip"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const sidebarGripLength = 3

// SidebarGuide is the overlay in frame coordinates, separate from the laid-out sidebar.
// End is exclusive.
type SidebarGuide struct {
	X, Start, End int
	Opacity       float64
}

// IsResizingSidebar reports whether the sidebar captured a resize press, so a
// motion and release over the bottom panel still go to the sidebar.
func IsResizingSidebar(page Page) bool {
	p, ok := page.(*chatPage)
	return ok && p.isDraggingSidebar
}

// ClearSidebarGripHover handles uncaptured motion outside chat.
func ClearSidebarGripHover(page Page) tea.Cmd {
	if p, ok := page.(*chatPage); ok {
		return p.setSidebarGrip(false, false)
	}
	return nil
}

// ResetSidebarGrip clears the visual state and capture when the page loses pointer access.
func ResetSidebarGrip(page Page) {
	if p, ok := page.(*chatPage); ok {
		p.isDraggingSidebar = false
		p.sidebarGuide.Cancel()
		p.sidebarGrip.Reset()
	}
}

// CancelSidebarDrag releases the pointer without applying the preview width.
func CancelSidebarDrag(page Page) tea.Cmd {
	if p, ok := page.(*chatPage); ok && p.isDraggingSidebar {
		return p.finishSidebarDrag(p.appliedLayout.sidebarWidth)
	}
	return nil
}

// SidebarResizeGuide reports the guide while dragging or settling.
func SidebarResizeGuide(page Page) (SidebarGuide, bool) {
	p, ok := page.(*chatPage)
	if !ok || (!p.isDraggingSidebar && !p.sidebarGuide.Running()) {
		return SidebarGuide{}, false
	}
	from := p.sidebarGuideFrom
	to := p.sidebarGuideTo
	if p.isDraggingSidebar {
		sl := p.appliedLayout
		to = SidebarGuide{X: styles.AppPadding + sl.dividerX(p.sidebarDragWidth), End: sl.chatHeight, Opacity: 1}
		if p.sidebarDragMoved {
			from.X = to.X // Horizontal motion follows the pointer without easing.
		}
	}
	progress := p.sidebarGuide.Value()
	lerp := func(from, to int) int { return from + int(math.Round(float64(to-from)*progress)) }
	return SidebarGuide{
		X:       lerp(from.X, to.X),
		Start:   lerp(from.Start, to.Start),
		End:     lerp(from.End, to.End),
		Opacity: from.Opacity + (to.Opacity-from.Opacity)*progress,
	}, true
}

func (p *chatPage) startSidebarDrag(x int) tea.Cmd {
	from, visible := SidebarResizeGuide(p)
	if !visible {
		start, end := sidebarGripRows(p.appliedLayout.chatHeight)
		from = SidebarGuide{X: x, Start: start, End: end, Opacity: 1}
	}
	p.sidebarGuideFrom = from
	p.isDraggingSidebar = true
	p.sidebarDragStartX = x
	p.sidebarDragWidth = p.appliedLayout.sidebarWidth
	p.sidebarDragMoved = false
	p.sidebarGrip.Reset()
	if p.ar == nil {
		return nil
	}
	return p.sidebarGuide.Start(animation.ShortDuration, animation.EaseOutCubic)
}

func (p *chatPage) sidebarDropWidth(x int, sl sidebarLayout) int {
	delta := x - p.sidebarDragStartX
	if !p.sidebarDragMoved && max(delta, -delta) < dragThreshold {
		return sl.sidebarWidth
	}
	if !sl.sidebarOnLeft {
		delta = -delta
	}
	width := sl.sidebarWidth + delta
	if width < sidebar.MinWidth {
		return 0
	}
	return p.sidebar.ClampWidth(width, sl.innerWidth)
}

func (p *chatPage) finishSidebarDrag(width int) tea.Cmd {
	from, _ := SidebarResizeGuide(p)
	sl := p.appliedLayout
	p.isDraggingSidebar = false
	p.sidebarGrip.Reset()

	var resize, toggle tea.Cmd
	if width != sl.sidebarWidth {
		p.sidebar.SetPreferredWidth(width)
		if width == 0 {
			p.sidebar.SetCollapsed(true)
			toggle = core.CmdHandler(msgtypes.ToggleSidebarMsg{})
		}
		resize = p.setSize(p.width, p.height, true)
	}

	var settle tea.Cmd
	if p.ar != nil && from.Start < from.End {
		x, height := styles.AppPadding+sl.dividerX(width), sl.chatHeight
		if p.appliedLayout.mode == sidebarVertical {
			x, height = styles.AppPadding+p.appliedLayout.handleX, p.appliedLayout.chatHeight
		}
		start, end := sidebarGripRows(height)
		p.sidebarGuideFrom = from
		p.sidebarGuideTo = SidebarGuide{X: x, Start: start, End: end}
		settle = p.sidebarGuide.Start(animation.ShortDuration, animation.EaseOutCubic)
	} else {
		p.sidebarGuide.Cancel()
	}
	return tea.Batch(resize, toggle, settle)
}

func (p *chatPage) updateSidebarGrip(x, y int) tea.Cmd {
	sl := p.computeSidebarLayout()
	adjustedX := x - styles.AppPadding
	// Reveal one cell to either side, without taking pointer events from the
	// adjacent pane. Only the divider's painted segment gets direct hover.
	nearEdge := sl.mode == sidebarVertical && y >= 0 && y < sl.chatHeight &&
		adjustedX >= sl.handleX-1 && adjustedX <= sl.handleX+1
	start, end := sidebarGripRows(sl.chatHeight)
	onGrip := sl.isOnHandle(adjustedX) && y >= start && y < end
	return p.setSidebarGrip(nearEdge, onGrip)
}

func (p *chatPage) setSidebarGrip(edge, direct bool) tea.Cmd {
	return p.sidebarGrip.Set(edge, direct, false)
}

func sidebarGripRows(height int) (start, end int) {
	// Keep row zero exclusively for the existing sidebar toggle.
	start, end = resizegrip.Segment(height-1, sidebarGripLength)
	return 1 + start, 1 + end
}

// renderSidebarHandle renders the toggle and compact grip in their shared
// column. Only the toggle glyph changes with the sidebar's position.
func (p *chatPage) renderSidebarHandle(height int) string {
	if height <= 0 {
		return ""
	}

	expandGlyph, collapseGlyph := "«", "»"
	if p.layoutSettings.SidebarPosition == msgtypes.SidebarLeft {
		expandGlyph, collapseGlyph = "»", "«"
	}
	glyph := collapseGlyph
	if p.sidebar.IsCollapsed() {
		glyph = expandGlyph
	}

	start, end := sidebarGripRows(height)
	return styles.MutedStyle.Render(glyph) + strings.Repeat("\n ", start-1) +
		strings.Repeat("\n"+p.sidebarGrip.Render("│", 1), end-start) +
		strings.Repeat("\n ", height-end)
}
