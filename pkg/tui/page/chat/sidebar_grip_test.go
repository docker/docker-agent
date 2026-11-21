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
	}{
		{msgtypes.SidebarLeft, "«", 1},
		{msgtypes.SidebarRight, "»", -1},
	} {
		t.Run(string(tc.position), func(t *testing.T) {
			p := newLayoutTestPage(t, tc.position)
			p.SetSize(p.width, p.height)
			sl := p.computeSidebarLayout()
			x := styles.AppPadding + sl.handleX
			tester := NewHitTest(p)
			rest := p.renderSidebarHandle(sl.chatHeight)
			lines := strings.Split(ansi.Strip(rest), "\n")
			require.Len(t, lines, sl.chatHeight)
			assert.Equal(t, tc.toggle, lines[0])
			assert.Equal(t, TargetSidebarToggle, tester.At(x, 0))
			assert.Equal(t, TargetSidebarResizeHandle, tester.At(x, 1))
			mid := sl.chatHeight / 2
			assert.Equal(t, tc.toggle+strings.Repeat("\n ", sl.chatHeight-1), ansi.Strip(rest))

			_, _ = p.handleMouseMotion(tea.MouseMotionMsg{X: x, Y: 0})
			edge := p.renderSidebarHandle(sl.chatHeight)
			assert.NotEqual(t, rest, edge)
			assert.Contains(t, edge, styles.ResizeHandleStyle.Render("│"))
			_, _ = p.handleMouseMotion(tea.MouseMotionMsg{X: x, Y: mid})
			assert.Contains(t, p.renderSidebarHandle(sl.chatHeight), styles.ResizeHandleHoverStyle.Render("│"))

			initial := p.GetSidebarSettings()
			_, _ = p.handleMouseClick(tea.MouseClickMsg{X: x, Y: 1, Button: tea.MouseLeft})
			require.True(t, IsResizingSidebar(p), "resize can start outside the visible grip")
			assert.Equal(t, rest, p.renderSidebarHandle(sl.chatHeight), "only the prospective guide is active")
			guideAt := func(wantX int) {
				t.Helper()
				guide, ok := SidebarResizeGuide(p)
				require.True(t, ok)
				assert.Equal(t, sidebarGripLength, guide.End-guide.Start)
				assert.Equal(t, wantX, guide.X)
				assert.Equal(t, initial, p.GetSidebarSettings())
				assert.Equal(t, sl, p.appliedLayout)
			}
			_, _ = p.handleMouseMotion(tea.MouseMotionMsg{X: x + tc.direction, Y: 1, Button: tea.MouseLeft})
			guideAt(x) // Below the resize threshold.
			_, _ = p.handleMouseMotion(tea.MouseMotionMsg{X: x - tc.direction*sl.sidebarWidth, Y: 1, Button: tea.MouseLeft})
			collapseX := styles.AppPadding
			if !sl.sidebarOnLeft {
				collapseX += sl.innerWidth - 1
			}
			guideAt(collapseX)
			_, _ = p.handleMouseMotion(tea.MouseMotionMsg{X: x + tc.direction*dragThreshold, Y: 1, Button: tea.MouseLeft})
			guideAt(x + tc.direction*dragThreshold)
			_, _ = p.handleMouseRelease(tea.MouseReleaseMsg{X: x + tc.direction*8, Y: p.height, Button: tea.MouseLeft})
			assert.False(t, IsResizingSidebar(p))
			assert.Equal(t, x+tc.direction*8, styles.AppPadding+p.appliedLayout.handleX, "drop uses release, not last motion")
			assert.Equal(t, sl.sidebarWidth+8, p.sidebar.GetPreferredWidth())
			assert.Equal(t, rest, p.renderSidebarHandle(sl.chatHeight))
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

func TestSidebarGripMarginRevealsWithoutHoverOrCapture(t *testing.T) {
	t.Parallel()
	for _, dock := range []msgtypes.SidebarPosition{msgtypes.SidebarLeft, msgtypes.SidebarRight} {
		t.Run(string(dock), func(t *testing.T) {
			p := newLayoutTestPage(t, dock)
			p.SetSize(p.width, p.height)
			sl := p.computeSidebarLayout()
			x, y := styles.AppPadding+sl.handleX, sl.chatHeight/2
			view := func() string { return p.renderSidebarHandle(sl.chatHeight) }
			rest := view()
			p.updateSidebarGrip(x, 0) // Divider, but not directly over the grip glyphs.
			revealed := view()
			require.NotEqual(t, rest, revealed)

			for _, direction := range []int{-1, 1} {
				p.updateSidebarGrip(x, y)
				require.Contains(t, view(), styles.ResizeHandleHoverStyle.Render("│"))
				_, _ = p.handleMouseMotion(tea.MouseMotionMsg{X: x + direction, Y: y})
				require.Equal(t, revealed, view(), "neighbor reveals, but loses direct hover")
				hit := NewHitTest(p).At(x+direction, y)
				if sl.isInSidebar(sl.handleX + direction) {
					require.Equal(t, TargetSidebarContent, hit)
				} else {
					require.Equal(t, TargetMessages, hit)
				}
				_, _ = p.handleMouseClick(tea.MouseClickMsg{X: x + direction, Y: y, Button: tea.MouseLeft})
				require.False(t, IsResizingSidebar(p))
				_, _ = p.handleMouseRelease(tea.MouseReleaseMsg{X: x + direction, Y: y, Button: tea.MouseLeft})
				p.updateSidebarGrip(x+2*direction, y)
				require.Equal(t, rest, view(), "two cells away is outside the reveal zone")
				p.updateSidebarGrip(x+direction, y)
				require.Equal(t, revealed, view(), "entering from outside reveals the grip")
				p.updateSidebarGrip(x+direction, sl.chatHeight)
				require.Equal(t, rest, view(), "the reveal margin does not extend below chat")
			}
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

func TestSidebarGripDimsOnlyAdjacentScrollbar(t *testing.T) {
	t.Parallel()
	p := newLayoutTestPage(t, msgtypes.SidebarRight)
	p.height = 12
	p.showStartupBanner = false
	queued := make([]string, 30)
	for i := range queued {
		queued[i] = "queued message"
		p.messages.AddUserMessage("chat line")
	}
	p.sidebar.SetQueuedMessages(queued...)
	p.SetSize(p.width, p.height)

	for _, dock := range []msgtypes.SidebarPosition{msgtypes.SidebarRight, msgtypes.SidebarLeft} {
		p.layoutSettings.SidebarPosition = dock
		p.SetSize(p.width, p.height)
		sl := p.appliedLayout
		messagesBefore, sidebarBefore := p.messages.View(), p.sidebar.View()
		require.Contains(t, ansi.Strip(messagesBefore), "│")
		require.Contains(t, ansi.Strip(sidebarBefore), "│")
		x := styles.AppPadding + sl.handleX
		p.updateSidebarGrip(x+1, 5)
		messagesAfter, sidebarAfter := p.messages.View(), p.sidebar.View()
		assert.Equal(t, ansi.Strip(messagesBefore), ansi.Strip(messagesAfter), "dimming does not change chat geometry or glyphs")
		assert.Equal(t, ansi.Strip(sidebarBefore), ansi.Strip(sidebarAfter), "dimming does not change sidebar geometry or glyphs")
		if dock == msgtypes.SidebarRight {
			assert.NotEqual(t, messagesBefore, messagesAfter)
			assert.Equal(t, sidebarBefore, sidebarAfter)
			p.layoutSettings.SidebarPosition = msgtypes.SidebarLeft
			p.SetSize(p.width, p.height)
			assert.Zero(t, p.messageScrollbarDim, "dock swap restores previous neighbor")
			assert.Positive(t, p.sidebarScrollbarDim, "dock swap dims the new neighbor")
		} else {
			assert.Equal(t, messagesBefore, messagesAfter)
			assert.NotEqual(t, sidebarBefore, sidebarAfter)
		}
		ClearSidebarGripHover(p)
		assert.Zero(t, p.messageScrollbarDim)
		assert.Zero(t, p.sidebarScrollbarDim)
	}

	p.updateSidebarGrip(styles.AppPadding+p.appliedLayout.handleX, 5)
	require.Positive(t, p.sidebarScrollbarDim)
	p.SetSize(minWindowWidth-1, p.height)
	assert.Zero(t, p.sidebarScrollbarDim, "narrow layout restores scrollbar paint")
	assert.Zero(t, p.sidebarGrip.Visibility())
}
