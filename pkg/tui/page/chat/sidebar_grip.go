package chat

import (
	"image"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/internal/resizegrip"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const sidebarGripLength = 3

// IsResizingSidebar reports whether the sidebar captured a resize press, so a
// motion and release over the bottom panel still go to the sidebar.
func IsResizingSidebar(page Page) bool {
	p, ok := page.(*chatPage)
	return ok && p.isDraggingSidebar
}

// SidebarDragImageMask covers the part of the original transcript width clipped during a drag.
func SidebarDragImageMask(page Page) image.Rectangle {
	p, ok := page.(*chatPage)
	if !ok || p.clippedChatColumns() == 0 {
		return image.Rectangle{}
	}
	sl := p.appliedLayout
	right := styles.AppPadding + sl.chatStartX + sl.chatWidth
	return image.Rect(right, 0, right+p.clippedChatColumns(), sl.chatHeight)
}

func (p *chatPage) clippedChatColumns() int {
	if !p.isDraggingSidebar {
		return 0
	}
	return max(0, p.sidebarDragWidth-p.sidebarDragStartWidth)
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
		CancelSidebarDrag(p)
		p.sidebarGrip.Reset()
	}
}

func CancelSidebarDrag(page Page) tea.Cmd {
	if p, ok := page.(*chatPage); ok && p.isDraggingSidebar {
		p.isDraggingSidebar = false
		return p.relayoutIfNeeded()
	}
	return nil
}

func (p *chatPage) startSidebarDrag(x int) {
	p.isDraggingSidebar = true
	p.sidebarDragStartX = x
	p.sidebarDragStartWidth = p.appliedLayout.sidebarWidth
	p.sidebarDragWidth = p.sidebarDragStartWidth
	p.sidebarDragMoved = false
}

func (p *chatPage) sidebarDropWidth(x int, sl sidebarLayout) int {
	delta := x - p.sidebarDragStartX
	if !p.sidebarDragMoved && max(delta, -delta) < dragThreshold {
		return p.sidebarDragStartWidth
	}
	if !sl.sidebarOnLeft {
		delta = -delta
	}
	width := p.sidebarDragStartWidth + delta
	if width < sidebar.MinWidth {
		return 0
	}
	return p.sidebar.ClampWidth(width, sl.innerWidth)
}

func (p *chatPage) finishSidebarDrag(width int) tea.Cmd {
	p.isDraggingSidebar = false
	var toggle tea.Cmd
	if width == p.sidebarDragStartWidth {
		return p.relayoutIfNeeded()
	}
	p.sidebar.SetPreferredWidth(width)
	if width == 0 {
		p.sidebar.SetCollapsed(true)
		toggle = core.CmdHandler(msgtypes.ToggleSidebarMsg{})
	}
	return tea.Batch(p.SetSize(p.width, p.height), toggle)
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
	var opacity float64
	if !p.isDraggingSidebar {
		if p.layoutSettings.SidebarPosition == msgtypes.SidebarLeft {
			opacity = p.sidebar.ScrollbarOpacity()
		} else {
			opacity = p.messages.ScrollbarOpacity()
		}
	}
	rule, grip := p.sidebarGrip.RenderOnSeparator(opacity)
	rule = "\n" + rule
	return styles.MutedStyle.Render(glyph) + strings.Repeat(rule, start-1) +
		strings.Repeat("\n"+grip, end-start) +
		strings.Repeat(rule, height-end)
}
