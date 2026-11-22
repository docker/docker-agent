package chat

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestSidebarGripPreservesResizeAndToggleTargets(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		position  msgtypes.SidebarPosition
		toggle    string
		direction int
	}{{msgtypes.SidebarLeft, "«", 1}, {msgtypes.SidebarRight, "»", -1}} {
		t.Run(string(tc.position), func(t *testing.T) {
			p := newLayoutTestPage(t, tc.position)
			p.SetSize(p.width, p.height)
			sl := p.appliedLayout
			x, mid := styles.AppPadding+sl.handleX, sl.chatHeight/2
			tester := NewHitTest(p)
			view := func() string { return p.renderSidebarHandle(sl.chatHeight) }
			rest := view()
			assert.Equal(t, tc.toggle+strings.Repeat("\n ", sl.chatHeight-1), ansi.Strip(rest))
			assert.Equal(t, TargetSidebarToggle, tester.At(x, 0))
			assert.Equal(t, TargetSidebarResizeHandle, tester.At(x, 1))

			p.updateSidebarGrip(x, mid)
			assert.Contains(t, view(), styles.ResizeHandleHoverStyle.Render("│"))
			for _, side := range []struct {
				dx     int
				target MouseTarget
			}{
				{tc.direction, TargetMessages}, {-tc.direction, TargetSidebarContent},
			} {
				p.updateSidebarGrip(x+side.dx, mid)
				assert.Contains(t, view(), styles.ResizeHandleStyle.Render("│"), "adjacent cell only reveals")
				assert.Equal(t, side.target, tester.At(x+side.dx, mid))
				p.handleMouseClick(tea.MouseClickMsg{X: x + side.dx, Y: mid, Button: tea.MouseLeft})
				require.False(t, IsResizingSidebar(p), "adjacent cell does not capture resize")
				p.handleMouseRelease(tea.MouseReleaseMsg{X: x + side.dx, Y: mid, Button: tea.MouseLeft})
				p.updateSidebarGrip(x+2*side.dx, mid)
				assert.Equal(t, rest, view(), "two cells away is outside the reveal zone")
			}
			p.updateSidebarGrip(x, sl.chatHeight)
			assert.Equal(t, rest, view(), "reveal does not extend below chat")

			p.handleMouseClick(tea.MouseClickMsg{X: x, Y: 1, Button: tea.MouseLeft})
			require.True(t, IsResizingSidebar(p), "exact divider captures outside the visible grip")
			for _, delta := range []int{dragThreshold - 1, dragThreshold} {
				p.handleMouseMotion(tea.MouseMotionMsg{X: x + tc.direction*delta, Y: 1, Button: tea.MouseLeft})
				guide, ok := SidebarResizeGuide(p)
				require.True(t, ok)
				wantX := x
				if delta == dragThreshold {
					wantX += tc.direction * delta
				}
				assert.Equal(t, wantX, guide.X, "drag delta %d", delta)
			}

			dropX := x + tc.direction*dragThreshold
			p.handleMouseRelease(tea.MouseReleaseMsg{X: dropX, Y: mid, Button: tea.MouseLeft})
			require.Equal(t, dropX, styles.AppPadding+p.appliedLayout.handleX)
			assert.Contains(t, view(), styles.ResizeHandleHoverStyle.Render("│"), "drop keeps hover without another motion event")
		})
	}
}

func TestSidebarDragStartsAtActualWidthAndCanCollapseOnDrop(t *testing.T) {
	t.Parallel()
	for _, dock := range []msgtypes.SidebarPosition{msgtypes.SidebarLeft, msgtypes.SidebarRight} {
		t.Run(string(dock), func(t *testing.T) {
			p := newLayoutTestPage(t, dock)
			p.sidebar.SetPreferredWidth(p.width) // Artificially wider than the window allows.
			p.SetSize(p.width, p.height)
			sl := p.appliedLayout
			initial := p.GetSidebarSettings()
			assert.Less(t, sl.sidebarWidth, initial.PreferredWidth)
			x, y := styles.AppPadding+sl.handleX, sl.chatHeight/2
			start := tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft}
			out := x - sl.sidebarWidth
			if !sl.sidebarOnLeft {
				out = x + sl.sidebarWidth
			}
			_, _ = p.handleMouseClick(start)
			_, _ = p.handleMouseMotion(tea.MouseMotionMsg{X: out, Y: y, Button: tea.MouseLeft})
			guide, ok := SidebarResizeGuide(p)
			require.True(t, ok)
			assert.Equal(t, styles.AppPadding+sl.dividerX(0), guide.X, "collapse previews at the outer edge")
			assert.Equal(t, initial, p.GetSidebarSettings())
			_, _ = p.handleMouseRelease(tea.MouseReleaseMsg(start))
			assert.Equal(t, initial, p.GetSidebarSettings(), "returning to origin preserves the unclamped preference")
			assert.Equal(t, sl, p.appliedLayout)

			_, _ = p.handleMouseClick(start)
			_, _ = p.handleMouseRelease(tea.MouseReleaseMsg{X: out, Y: -1, Button: tea.MouseLeft})
			assert.False(t, IsResizingSidebar(p))
			assert.Equal(t, SidebarSettings{Collapsed: true}, p.GetSidebarSettings(), "release-only motion can collapse")
			assert.Equal(t, sidebarCollapsed, p.appliedLayout.mode)
		})
	}
}

func TestSidebarGripLeavesToggleAloneOnShortColumn(t *testing.T) {
	t.Parallel()
	p := newLayoutTestPage(t, msgtypes.SidebarRight)
	assert.Empty(t, p.renderSidebarHandle(0))
	assert.Equal(t, "»", ansi.Strip(p.renderSidebarHandle(1)))
	assert.Equal(t, "»\n ", ansi.Strip(p.renderSidebarHandle(2)))
}

func TestSidebarBandKeepsBlankSeparatorRow(t *testing.T) {
	t.Parallel()
	for _, dock := range []msgtypes.SidebarPosition{msgtypes.SidebarTop, msgtypes.SidebarBottom, msgtypes.SidebarRight} {
		t.Run(string(dock), func(t *testing.T) {
			p := newLayoutTestPage(t, dock)
			p.sidebar.SetCollapsed(true)
			p.SetSize(p.width, p.height)
			sl := p.computeSidebarLayout()
			lines := strings.Split(ansi.Strip(p.renderCollapsedSidebar(sl)), "\n")
			require.Len(t, lines, sl.sidebarHeight)
			y := sl.sidebarHeight - 1
			if sl.bandAtBottom {
				y = 0
			}
			require.Equal(t, strings.Repeat(" ", sl.innerWidth), lines[y])
		})
	}
}
