package chat

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/scrollview"
	tuiimage "github.com/docker/docker-agent/pkg/tui/image"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
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
			divider := styles.ResizeHandleStyle.Foreground(styles.Separator).Render("│")
			assert.Equal(t, styles.MutedStyle.Render(tc.toggle)+strings.Repeat("\n"+divider, sl.chatHeight-1), rest)
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
				near := strings.Split(view(), "\n")[mid]
				assert.NotEqual(t, divider, near)
				assert.NotEqual(t, styles.ResizeHandleHoverStyle.Render("│"), near)
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
				wantX := x
				if delta == dragThreshold {
					wantX += tc.direction * delta
				}
				assert.Equal(t, wantX, styles.AppPadding+p.appliedLayout.handleX, "drag delta %d", delta)
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
	assert.Equal(t, "»\n│", ansi.Strip(p.renderSidebarHandle(2)))
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

func TestSidebarDividerCrossfadesWithTranscriptScrollbar(t *testing.T) {
	t.Parallel()
	p := newLayoutTestPage(t, msgtypes.SidebarRight)
	p.SetSize(p.width, p.height)
	sv := p.messages.(interface{ ScrollbarView() *scrollview.Model }).ScrollbarView()
	ar := animation.NewRuntimeWithScheduler(&bannerScheduler{now: time.Unix(1, 0)})
	sv.SetAnimationRuntime(ar)
	sv.SetContent(strings.Split(strings.Repeat("row\n", 100), "\n"), 101)
	view := func() string { return strings.Split(p.renderSidebarHandle(p.height), "\n")[1] }
	idle := view()
	cmd := sv.SetScrollbarPointer(sv.ScrollbarX(), 2)
	step := func() {
		t.Helper()
		require.NotNil(t, cmd)
		tick, ok := ar.Accept(cmd().(animation.TickMsg))
		require.True(t, ok)
		sv.UpdateScrollbarAnimation(tick)
		cmd = ar.Continue()
	}
	settle := func() {
		for cmd != nil {
			step()
		}
	}
	step()
	require.NotEqual(t, idle, view())
	require.NotEqual(t, " ", view())
	settle()
	require.Equal(t, " ", view())
	x := styles.AppPadding + p.appliedLayout.handleX
	_, cmd = p.handleMouseClick(tea.MouseClickMsg{X: x, Y: p.height / 2, Button: tea.MouseLeft})
	require.True(t, p.isDraggingSidebar)
	require.Equal(t, idle, view(), "the edge is visible from the press")
	p.SetScrollbarPointer(sv.ScrollbarX(), 2)
	step()
	require.Positive(t, sv.ScrollbarOpacity())
	require.Less(t, sv.ScrollbarOpacity(), 1.0)
	settle()
	require.Zero(t, sv.ScrollbarOpacity(), "nearby pointer cannot reveal messages scrollbar during capture")
	_, cmd = p.handleMouseRelease(tea.MouseReleaseMsg{X: x, Y: p.height / 2, Button: tea.MouseLeft})
	settle()
	require.Equal(t, " ", view())
	p.updateSidebarGrip(styles.AppPadding+p.appliedLayout.handleX, p.height/2)
	require.Contains(t, p.renderSidebarHandle(p.height), styles.ResizeHandleHoverStyle.Render("│"))
	p.layoutSettings.SidebarPosition = msgtypes.SidebarLeft
	require.Equal(t, idle, view(), "a scrollbar on the opposite outer edge does not hide the divider")
	p.layoutSettings.SidebarPosition = msgtypes.SidebarRight
	cmd = sv.SetScrollbarPointer(-1, -1)
	settle()
	require.Equal(t, idle, view())
}

func TestSidebarDragDefersTranscriptReflow(t *testing.T) {
	t.Parallel()
	p := newLayoutTestPage(t, msgtypes.SidebarRight)
	p.SetSize(p.width, p.height)
	dims := p.messages.(interface{ GetSize() (int, int) })
	original, _ := dims.GetSize()
	p.messages.AddUserMessage(strings.Repeat("long line ", 100))
	x := styles.AppPadding + p.appliedLayout.handleX
	p.startSidebarDrag(x)
	for _, dx := range []int{-5, -12, -7} {
		p.handleMouseMotion(tea.MouseMotionMsg{X: x + dx, Button: tea.MouseLeft})
		w, _ := dims.GetSize()
		require.Equal(t, original, w, "do not invalidate every message width on motion")
		require.Equal(t, original+dx, p.appliedLayout.chatWidth)
		require.Len(t, strings.Split(p.View(), "\n"), p.height)
	}
	p.handleMouseRelease(tea.MouseReleaseMsg{X: x - 7, Button: tea.MouseLeft})
	w, _ := dims.GetSize()
	require.Equal(t, original-7, w)
}

func TestSidebarDragCropsInlineImage(t *testing.T) {
	t.Parallel()
	pattern := regexp.MustCompile(`\x1b\[\d+;(\d+)H\x1b_Ga=p,[^\x1b]*?,c=(\d+),r=\d+([^\x1b]*)`)
	for _, dock := range []msgtypes.SidebarPosition{msgtypes.SidebarLeft, msgtypes.SidebarRight} {
		t.Run(string(dock), func(t *testing.T) {
			p := newLayoutTestPage(t, dock)
			p.SetSize(120, 30)
			const pixels = 600
			var data, output bytes.Buffer
			require.NoError(t, png.Encode(&data, image.NewGray(image.Rect(0, 0, pixels, 40))))
			p.messages.AppendAssistantMedia("root", []types.AssistantMedia{{Image: &tuiimage.Inline{Name: "wide.png", PNGData: data.Bytes(), Width: pixels, Height: 40}}})
			writer := tuiimage.NewWriter(&output)
			placement := func() (x, cols int, params string) {
				t.Helper()
				output.Reset()
				writer.SetContent(p.View(), tuiimage.ForegroundLayer{Bounds: SidebarDragImageMask(p)})
				_, err := writer.Write([]byte("frame"))
				require.NoError(t, err)
				match := pattern.FindStringSubmatch(output.String())
				require.Len(t, match, 4)
				x, err = strconv.Atoi(match[1])
				require.NoError(t, err)
				cols, err = strconv.Atoi(match[2])
				require.NoError(t, err)
				return x - 1, cols, match[3]
			}
			_, full, _ := placement()
			x := styles.AppPadding + p.appliedLayout.handleX
			p.startSidebarDrag(x)
			delta := 40
			if dock == msgtypes.SidebarRight {
				delta = -delta
			}
			p.handleMouseMotion(tea.MouseMotionMsg{X: x + delta, Button: tea.MouseLeft})
			start, cols, params := placement()
			sl := p.appliedLayout
			require.Equal(t, styles.AppPadding+sl.chatStartX+sl.chatWidth, start+cols)
			require.Less(t, cols, full)
			require.Contains(t, params, fmt.Sprintf(",w=%d,", pixels*cols/full))
			CancelSidebarDrag(p)
			_, restored, _ := placement()
			require.Equal(t, full, restored)
		})
	}
}
