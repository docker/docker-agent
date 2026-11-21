package tui

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentruntime "github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/internal/ansiline"
	"github.com/docker/docker-agent/pkg/tui/internal/resizegrip"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// TestRenderResizeHandle_TinyWidths guards against a panic (negative
// strings.Repeat count) when the terminal reports degenerate sizes such as
// 0x0 or 1x1: the padded inner width goes negative for widths smaller than
// the app's horizontal padding.
func TestRenderResizeHandle_TinyWidths(t *testing.T) {
	t.Parallel()

	m, _ := newTestModel(t)

	for _, width := range []int{-1, 0, 1, appPaddingHorizontal, appPaddingHorizontal + 1, 80} {
		out := m.renderResizeHandle(width)
		if width <= appPaddingHorizontal {
			assert.Empty(t, out, "width %d", width)
		} else {
			assert.Equal(t, strings.Repeat(" ", width), out, "idle divider at width %d", width)
		}
	}
}

func TestComposerGripKeepsFullWidthResizeHitArea(t *testing.T) {
	m, _, _ := harnessRoot(t, 160, 40, animation.NewRuntimeWithScheduler(frozenScheduler{}))
	// Snap to hover colors here; timed transitions are covered by resizegrip's tests.
	m.composerGrip = resizegrip.Reveal{}

	rest := m.renderResizeHandle(m.width)
	require.Equal(t, strings.Repeat(" ", m.width), rest)
	start := (m.width - resizeHandleWidth) / 2
	segment := strings.Repeat("─", resizeHandleWidth)
	visible := strings.Repeat(" ", start) + segment + strings.Repeat(" ", m.width-start-resizeHandleWidth)

	row := m.contentHeight
	_, _ = m.Update(tea.MouseMotionMsg{X: start - 1, Y: row})
	edge := m.renderResizeHandle(m.width)
	assert.Equal(t, visible, ansi.Strip(edge), "reveal only the compact segment, not a full-width rule")
	_, _ = m.Update(tea.MouseMotionMsg{X: start, Y: row})
	hover := m.renderResizeHandle(m.width)
	assert.Contains(t, hover, styles.ResizeHandleHoverStyle.Render(strings.Repeat("─", resizeHandleWidth)))
	assert.Equal(t, visible, ansi.Strip(hover))

	_, _ = m.Update(tea.MouseClickMsg{X: 0, Y: row, Button: tea.MouseLeft})
	require.True(t, m.isDragging, "the full row, including its padded edge, remains draggable")
	assert.NotEqual(t, edge, m.renderResizeHandle(m.width), "press darkens the grip")
	assert.Equal(t, visible, ansi.Strip(m.renderResizeHandle(m.width)))
	_, _ = m.Update(tea.MouseMotionMsg{X: 0, Y: row - 3, Button: tea.MouseLeft})
	assert.Less(t, m.contentHeight, row)
	_, _ = m.Update(tea.MouseReleaseMsg{X: 0, Y: 0, Button: tea.MouseLeft})
	assert.False(t, m.isDragging)
	assert.Equal(t, rest, m.renderResizeHandle(m.width))
}

// Exercise the actual rendered divider, not just the editor's row count.
func TestComposerDragKeepsGripUnderPointer(t *testing.T) {
	for _, tc := range []struct {
		name string
		tabs []messages.TabInfo
	}{
		{"single", []messages.TabInfo{{SessionID: "profile", Title: "One"}}},
		{"tab strip", []messages.TabInfo{{SessionID: "profile", Title: "One"}, {SessionID: "second", Title: "Two"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _, _ := frozenClockRoot(t, 160, 40)
			m.tabBar.SetTabs(tc.tabs, 0)
			m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
			x, initial := m.width/2, m.contentHeight
			m.Update(tea.MouseClickMsg{X: x, Y: initial, Button: tea.MouseLeft})
			require.True(t, m.isDragging)
			move := func(y int) {
				t.Helper()
				m.Update(tea.MouseMotionMsg{X: x, Y: y, Button: tea.MouseLeft})
			}
			at := func(y int) {
				t.Helper()
				require.Equal(t, regionResizeHandle, m.hitTestRegion(y), "pointer row %d", y)
				lines := strings.Split(ansi.Strip(m.View().Content), "\n")
				require.Contains(t, lines[y], strings.Repeat("─", resizeHandleWidth), "painted row %d", y)
			}
			for _, dy := range []int{0, -1, -4, -4, -2, 0} {
				move(initial + dy)
				at(initial + dy)
			}
			move(0) // Clamp, then move back into the available range without drift.
			require.Positive(t, m.contentHeight)
			clamped := m.View().Content
			move(1)
			require.True(t, m.viewCacheValid, "unchanged clamped motion reuses the root frame")
			require.Equal(t, clamped, m.View().Content)
			move(initial - 3)
			m.Update(tea.MouseReleaseMsg{X: x, Y: initial - 3, Button: tea.MouseLeft})
			require.False(t, m.isDragging)
			at(initial - 3)
		})
	}
}

func TestComposerSeparatorRemainsBlankDuringActivity(t *testing.T) {
	m, _, _ := frozenClockRoot(t, 160, 40)
	m.Update(agentruntime.StreamStarted("profile", "root"))
	require.True(t, m.activeTab.chatPage.IsWorking())
	for _, size := range [][2]int{{160, 40}, {48, 20}, {24, 12}} {
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		for _, pause := range []service.PauseState{service.PauseNone, service.PausePausing, service.PausePaused} {
			m.activeTab.sessionState.SetPauseState(pause)
			m.viewCacheValid = false
			row := strings.Split(ansi.Strip(m.View().Content), "\n")[m.contentHeight]
			assert.Empty(t, strings.TrimSpace(row))
			assert.Equal(t, strings.Repeat(" ", m.width), m.renderResizeHandle(m.width))
			assert.Equal(t, regionResizeHandle, m.hitTestRegion(m.contentHeight))
			assert.False(t, m.composerResizeHit(m.width/2, m.editorTop()+styles.EditorStyle.GetPaddingTop()))
		}
	}
}

func TestComposerDefaultIsReachableAndPointerAccessCanBeLost(t *testing.T) {
	for _, cancel := range []string{"release outside", "blur", "modal"} {
		t.Run(cancel, func(t *testing.T) {
			m, _, _ := frozenClockRoot(t, 160, 40)
			m.composerGrip = resizegrip.Reveal{}
			_, originalHeight := m.activeTab.editor.GetSize()
			require.Equal(t, 1+styles.EditorStyle.GetVerticalFrameSize(), originalHeight)
			x, y := m.width/2, m.contentHeight
			m.Update(tea.MouseClickMsg{X: -1, Y: y, Button: tea.MouseLeft})
			require.False(t, m.isDragging)
			m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
			require.True(t, m.isDragging)
			m.Update(tea.MouseMotionMsg{X: x, Y: y, Button: tea.MouseLeft})
			require.Equal(t, y, m.contentHeight)
			m.Update(tea.MouseMotionMsg{X: x, Y: y - 5, Button: tea.MouseLeft})
			require.Equal(t, y-5, m.contentHeight)
			m.Update(tea.MouseMotionMsg{X: x, Y: m.height - 1, Button: tea.MouseLeft})
			require.Equal(t, y, m.contentHeight, "manual minimum matches the compact starting height")
			switch cancel {
			case "release outside":
				m.Update(tea.MouseReleaseMsg{X: -1, Y: y, Button: tea.MouseLeft})
			case "blur":
				m.Update(tea.BlurMsg{})
			case "modal":
				m.updateDialogCmd(dialog.OpenDialogMsg{Model: &stubDialog{id: "resize-occlusion"}})
			}
			require.False(t, m.isDragging)
			assert.Equal(t, strings.Repeat(" ", m.width), m.renderResizeHandle(m.width))
			m.Update(tea.WindowSizeMsg{Width: 1, Height: 1})
			assert.False(t, m.composerResizeHit(0, m.contentHeight))
			assert.NotPanics(t, func() { m.View() })
		})
	}
}

func TestComposerResizeKeepsAttachmentAndTextRowsSeparate(t *testing.T) {
	m, _, _ := frozenClockRoot(t, 160, 40)
	file := filepath.Join(t.TempDir(), "attachment.txt")
	require.NoError(t, os.WriteFile(file, []byte("attached"), 0o600))
	initial := m.contentHeight
	m.Update(messages.InsertFileRefMsg{FilePath: file})
	require.Equal(t, 1, m.activeTab.editor.BannerHeight())
	y := initial - 1
	require.Equal(t, y, m.contentHeight, "banner increases the composer without stealing its text row")
	x := m.width / 2
	m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.True(t, m.isDragging)
	m.Update(tea.MouseMotionMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.Equal(t, y, m.contentHeight)
	m.Update(tea.MouseMotionMsg{X: x, Y: y - 4, Button: tea.MouseLeft})
	require.Equal(t, y-4, m.contentHeight)
	m.Update(tea.MouseMotionMsg{X: x, Y: m.height - 1, Button: tea.MouseLeft})
	require.Equal(t, y, m.contentHeight)
}

func TestSidebarGripFadesWhenPointerLeavesChatAndReleasesOutside(t *testing.T) {
	ar := animation.NewRuntimeWithScheduler(&rootImmediateScheduler{now: time.Unix(1, 0)})
	m, _, _ := harnessRoot(t, 160, 40, ar)
	p := chat.New(ar, t.Context(), m.application, m.activeTab.sessionState)
	m.activeTab.chatPage = p
	t.Cleanup(func() { chat.Cleanup(p) })
	_, _ = m.Update(tea.WindowSizeMsg{Width: m.width, Height: m.height})

	rest := m.View().Content

	// The toggle marks the same divider column even when the grip is invisible.
	first, _, _ := strings.Cut(ansi.Strip(rest), "\n")
	before, _, found := strings.Cut(first, "»")
	require.True(t, found, "right sidebar toggle in first row")
	x, y := lipgloss.Width(before), m.contentHeight/2
	advance := func(cmd tea.Cmd) {
		t.Helper()
		for i := 0; ar.HasActive(); i++ {
			require.Less(t, i, 3)
			var next tea.Cmd
			for _, msg := range collectMsgs(cmd) {
				if tick, ok := msg.(animation.TickMsg); ok {
					_, next = m.Update(tick)
				}
			}
			cmd = next
		}
	}

	_, cmd := m.Update(tea.MouseMotionMsg{X: x, Y: y})
	require.True(t, ar.HasActive())
	advance(cmd)
	assert.Equal(t, "│", ansi.Strip(ansi.Cut(strings.Split(m.View().Content, "\n")[y], x, x+1)), "only revealed while pointer is near")
	_, cmd = m.Update(tea.MouseMotionMsg{X: x, Y: m.height - 1})
	require.True(t, ar.HasActive())
	advance(cmd)
	assert.Equal(t, rest, m.View().Content, "sidebar must fade without another motion inside chat")

	_, cmd = m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.True(t, chat.IsResizingSidebar(p))
	_, _ = m.Update(tea.MouseReleaseMsg{X: x, Y: m.height - 1, Button: tea.MouseLeft})
	assert.False(t, chat.IsResizingSidebar(p))
	for m.ar.HasActive() {
		cmd = sidebarGuideTestTick(t, m, cmd)
	}
	assert.Equal(t, rest, m.View().Content)
}

type sidebarGuideScheduler struct {
	now  time.Time
	step time.Duration
}

func (s *sidebarGuideScheduler) Now() time.Time { return s.now }
func (s *sidebarGuideScheduler) Tick(_ time.Duration, f func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg { s.now = s.now.Add(s.step); return f(s.now) }
}

func sidebarResizeTestRoot(t *testing.T, position messages.SidebarPosition) (*appModel, int, *sidebarGuideScheduler) {
	t.Helper()
	scheduler := &sidebarGuideScheduler{now: time.Unix(1, 0), step: animation.ShortDuration}
	ar := animation.NewRuntimeWithScheduler(scheduler)
	m, _, _ := harnessRoot(t, 160, 40, ar)
	chat.Cleanup(m.activeTab.chatPage)
	p := chat.New(m.ar, t.Context(), m.application, m.activeTab.sessionState,
		chat.WithShowBanner(false), chat.WithLayoutSettings(messages.LayoutSettings{SidebarPosition: position}))
	m.activeTab.chatPage = p
	t.Cleanup(func() { chat.Cleanup(p) })
	p.SetSidebarSettings(chat.SidebarSettings{PreferredWidth: 45})
	m.Update(tea.WindowSizeMsg{Width: m.width, Height: m.height})
	return m, renderedSidebarDivider(t, m.View().Content, position), scheduler
}

func renderedSidebarDivider(t *testing.T, frame string, position messages.SidebarPosition) int {
	t.Helper()
	glyph := "»"
	if position == messages.SidebarLeft {
		glyph = "«"
	}
	first, _, _ := strings.Cut(ansi.Strip(frame), "\n")
	before, _, ok := strings.Cut(first, glyph)
	require.True(t, ok, "sidebar toggle not drawn")
	return lipgloss.Width(before)
}

func TestSidebarResizeGuideOverlaysLiveFrameWithoutRelayout(t *testing.T) {
	for _, dock := range []messages.SidebarPosition{messages.SidebarLeft, messages.SidebarRight} {
		t.Run(string(dock), func(t *testing.T) {
			m, x, scheduler := sidebarResizeTestRoot(t, dock)
			y := m.contentHeight / 2
			direction := 1
			if dock == messages.SidebarRight {
				direction = -1
			}
			// Read the initial span from the actual compact grip, not the click row.
			_, hoverCmd := m.Update(tea.MouseMotionMsg{X: x, Y: y})
			for m.ar.HasActive() {
				hoverCmd = sidebarGuideTestTick(t, m, hoverCmd)
			}
			gripStart, gripEnd := -1, -1
			for row, line := range strings.Split(ansi.Strip(m.View().Content), "\n") {
				if ansi.Cut(line, x, x+1) == "│" {
					if gripStart < 0 {
						gripStart = row
					}
					gripEnd = row + 1
				}
			}
			require.Equal(t, 3, gripEnd-gripStart)
			assertGuide := func(previewX, start, end int) string {
				t.Helper()
				frame := m.View().Content
				rows := strings.Split(ansi.Strip(frame), "\n")
				base := strings.Split(ansi.Strip(m.viewCache.Content), "\n")
				require.Len(t, rows, len(base))
				assert.Equal(t, x, renderedSidebarDivider(t, m.viewCache.Content, dock), "actual divider stays put")
				for row := range m.contentHeight {
					if row >= start && row < end {
						assert.Equal(t, "│", ansi.Cut(rows[row], previewX, previewX+1), "guide row %d", row)
						assert.Equal(t, lipgloss.Width(base[row]), lipgloss.Width(rows[row]))
					} else {
						assert.Equal(t, base[row], rows[row], "row %d outside guide", row)
					}
				}
				assert.Equal(t, base[m.contentHeight:], rows[m.contentHeight:], "lower panels stay untouched")
				return frame
			}
			initial := m.activeTab.chatPage.GetSidebarSettings()
			scheduler.step = animation.ShortDuration / 4
			_, cmd := m.Update(tea.MouseClickMsg{X: x, Y: 1, Button: tea.MouseLeft})
			assertGuide(x, gripStart, gripEnd)
			base := m.viewCache.Content
			generation := m.activeTab.chatPage.VisualGeneration()
			move := tea.MouseMotionMsg{X: x + 5*direction, Y: m.height + 1, Button: tea.MouseLeft}
			m.Update(move)
			require.True(t, m.viewCacheValid, "moving the guide does not invalidate the root cache")
			preview := assertGuide(move.X, gripStart, gripEnd)
			m.Update(move)
			require.True(t, m.viewCacheValid, "repeating a held motion leaves the root cache intact")
			assert.Equal(t, preview, m.View().Content)
			cmd = sidebarGuideTestTick(t, m, cmd)
			require.True(t, m.viewCacheValid, "guide-only ticks don't invalidate the root cache")
			mid := m.View().Content
			require.NotEqual(t, preview, mid)
			guide := requireSidebarGuide(t, m)
			expected := animation.EaseOutCubic(0.25)
			assert.Equal(t, gripStart-int(math.Round(float64(gripStart)*expected)), guide.Start)
			assert.Equal(t, gripEnd+int(math.Round(float64(m.contentHeight-gripEnd)*expected)), guide.End)
			assertGuide(move.X, guide.Start, guide.End)
			scheduler.step = animation.ShortDuration - scheduler.step
			cmd = sidebarGuideTestTick(t, m, cmd)
			assert.Nil(t, cmd)
			assert.False(t, m.ar.HasActive(), "a held, fully extended guide is idle")
			require.True(t, m.viewCacheValid)
			assert.Equal(t, base, m.viewCache.Content)
			assert.Equal(t, generation, m.activeTab.chatPage.VisualGeneration(), "preview doesn't touch the transcript")
			assertGuide(move.X, 0, m.contentHeight)
			m.Update(tea.MouseMotionMsg{X: x, Y: y, Button: tea.MouseLeft})
			assertGuide(x, 0, m.contentHeight) // At the actual divider the guide paints over the toggle.
			m.Update(move)
			m.Update(agentruntime.UserMessage("LIVE-RESIZE-PREVIEW-MARKER", "profile", nil))
			assert.Contains(t, ansi.Strip(assertGuide(move.X, 0, m.contentHeight)), "-RESIZE-PREVIEW-MARKER", "underlying frame stays live")
			assert.Equal(t, initial, m.activeTab.chatPage.GetSidebarSettings())
			shown := requireSidebarGuide(t, m)
			_, release := m.Update(tea.MouseReleaseMsg{X: x + 7*direction, Y: -1, Button: tea.MouseLeft})
			assert.False(t, chat.IsResizingSidebar(m.activeTab.chatPage))
			assert.Equal(t, initial.PreferredWidth+7, m.activeTab.chatPage.GetSidebarSettings().PreferredWidth)
			assert.Equal(t, shown, requireSidebarGuide(t, m), "release starts at the last visible preview, not the release position")
			_ = m.View()
			assert.Equal(t, x+7*direction, renderedSidebarDivider(t, m.viewCache.Content, dock))
			assertSidebarGuideSettles(t, m, scheduler, release, chat.SidebarGuide{X: x + 7*direction, Start: gripStart, End: gripEnd})
		})
	}
}

func sidebarGuideTestTick(t *testing.T, m *appModel, cmd tea.Cmd) tea.Cmd {
	t.Helper()
	var tick animation.TickMsg
	var found bool
	for _, msg := range collectMsgs(cmd) {
		if next, ok := msg.(animation.TickMsg); ok {
			require.False(t, found, "guide uses exactly one shared tick chain")
			tick, found = next, true
		}
	}
	require.True(t, found, "active animation did not schedule a tick")
	_, next := m.Update(tick)
	return next
}

func requireSidebarGuide(t *testing.T, m *appModel) chat.SidebarGuide {
	t.Helper()
	guide, ok := chat.SidebarResizeGuide(m.activeTab.chatPage)
	require.True(t, ok)
	return guide
}

func assertSidebarGuideSettles(t *testing.T, m *appModel, scheduler *sidebarGuideScheduler, cmd tea.Cmd, target chat.SidebarGuide) {
	t.Helper()
	from := requireSidebarGuide(t, m)
	_ = m.View()
	base := m.viewCache.Content
	generation := m.activeTab.chatPage.VisualGeneration()
	startedAt := m.ar.Now()
	scheduler.step = animation.ShortDuration / 3
	cmd = sidebarGuideTestTick(t, m, cmd)
	eased := animation.EaseOutCubic(float64(scheduler.step) / float64(animation.ShortDuration))
	lerp := func(a, b int) int { return a + int(math.Round(float64(b-a)*eased)) }
	mid := requireSidebarGuide(t, m)
	assert.Equal(t, lerp(from.X, target.X), mid.X)
	assert.Equal(t, lerp(from.Start, target.Start), mid.Start)
	assert.Equal(t, lerp(from.End, target.End), mid.End)
	assert.InDelta(t, from.Opacity*(1-eased), mid.Opacity, 1e-9)
	assert.Positive(t, mid.Opacity)
	assert.Less(t, mid.Opacity, from.Opacity)
	require.True(t, m.viewCacheValid, "settling does not invalidate the live base frame")
	midFrame := strings.Split(m.View().Content, "\n")
	baseRows := strings.Split(base, "\n")
	assert.Equal(t, ansiline.Splice(baseRows[mid.Start], resizegrip.RenderGuide(mid.Opacity), mid.X), midFrame[mid.Start])
	assert.NotEqual(t, ansiline.Splice(baseRows[mid.Start], resizegrip.RenderGuide(from.Opacity), mid.X), midFrame[mid.Start])
	scheduler.step = animation.ShortDuration - (m.ar.Now() - startedAt)
	assert.Nil(t, sidebarGuideTestTick(t, m, cmd))
	assert.Equal(t, animation.ShortDuration, m.ar.Now()-startedAt)
	assert.False(t, m.ar.HasActive(), "settled guide releases the clock")
	require.True(t, m.viewCacheValid)
	assert.Equal(t, generation, m.activeTab.chatPage.VisualGeneration())
	assert.Equal(t, base, m.View().Content, "settled guide is fully invisible")
}

func TestSidebarResizeGuideSettlesWithoutCommitting(t *testing.T) {
	for _, tc := range []struct {
		name    string
		elapsed time.Duration
		msg     func(x, y int) tea.Msg
	}{
		{"escape mid-growth", animation.ShortDuration / 5, func(_, _ int) tea.Msg { return tea.KeyPressMsg{Code: tea.KeyEscape} }},
		{"buttonless motion", animation.ShortDuration, func(x, y int) tea.Msg { return tea.MouseMotionMsg{X: x, Y: y} }},
		{"immediate release at origin", 0, func(x, y int) tea.Msg { return tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, x, scheduler := sidebarResizeTestRoot(t, messages.SidebarRight)
			y := m.contentHeight / 2
			p := m.activeTab.chatPage
			initial := p.GetSidebarSettings()
			_, queued := m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
			compact := requireSidebarGuide(t, m)
			m.Update(tea.MouseMotionMsg{X: m.width + 3, Y: y, Button: tea.MouseLeft})
			if tc.elapsed > 0 {
				scheduler.step = tc.elapsed
				queued = sidebarGuideTestTick(t, m, queued)
			}
			preview := requireSidebarGuide(t, m)
			assert.Equal(t, m.width-styles.AppPadding-1, preview.X, "collapse previews at the outer edge")
			row := strings.Split(ansi.Strip(m.View().Content), "\n")[y]
			require.Equal(t, "│", ansi.Cut(row, preview.X, preview.X+1))
			_, cmd := m.Update(tc.msg(x, y))
			require.False(t, chat.IsResizingSidebar(p))
			assert.Equal(t, preview, requireSidebarGuide(t, m), "settling starts at the exact preview")
			_, _ = m.Update(tea.MouseReleaseMsg{X: m.width + 3, Y: y, Button: tea.MouseLeft})
			assert.Equal(t, initial, p.GetSidebarSettings(), "no change on cancellation or a stale release")
			assert.Equal(t, x, renderedSidebarDivider(t, m.View().Content, messages.SidebarRight))
			if queued != nil {
				assert.Nil(t, cmd, "interruption reuses the pending shared tick")
				cmd = queued
			}
			compact.Opacity = 0
			assertSidebarGuideSettles(t, m, scheduler, cmd, compact)
		})
	}
}

func TestSidebarResizeGuideRegrabWhileSettling(t *testing.T) {
	m, x, scheduler := sidebarResizeTestRoot(t, messages.SidebarRight)
	y := m.contentHeight / 2
	p := m.activeTab.chatPage
	_, cmd := m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	compact := requireSidebarGuide(t, m)
	m.Update(tea.MouseMotionMsg{X: x - 10, Y: y, Button: tea.MouseLeft})
	require.Nil(t, sidebarGuideTestTick(t, m, cmd))
	_, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	scheduler.step = animation.ShortDuration / 6
	cmd = sidebarGuideTestTick(t, m, cmd)
	settling := requireSidebarGuide(t, m)
	frame := m.View().Content
	_, next := m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	assert.Nil(t, next, "the pending tick drives the new growth")
	require.True(t, chat.IsResizingSidebar(p))
	assert.Equal(t, settling, requireSidebarGuide(t, m))
	assert.Equal(t, frame, m.View().Content, "re-grabbing does not flash or reset the visible span")
	m.Update(tea.MouseMotionMsg{X: x - 6, Y: y, Button: tea.MouseLeft})
	assert.Equal(t, x-6, requireSidebarGuide(t, m).X, "the new motion is immediate")
	from := requireSidebarGuide(t, m)
	_, next = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.Nil(t, next)
	assert.Equal(t, from, requireSidebarGuide(t, m), "a second interruption preserves the partial opacity")
	compact.Opacity = 0
	assertSidebarGuideSettles(t, m, scheduler, cmd, compact)
}

func TestSidebarResizeGuideClearsOnNativeVisibilityLoss(t *testing.T) {
	for _, tc := range []struct {
		name string
		lose func(m *appModel)
	}{
		{"blur", func(m *appModel) { m.Update(tea.BlurMsg{}) }},
		{"hidden", func(m *appModel) { m.setPaneHidden(true) }},
	} {
		for _, settling := range []bool{false, true} {
			phase := "growing"
			if settling {
				phase = "settling"
			}
			t.Run(tc.name+"/"+phase, func(t *testing.T) {
				m, x, scheduler := sidebarResizeTestRoot(t, messages.SidebarRight)
				p := m.activeTab.chatPage
				initial := p.GetSidebarSettings()
				_, queued := m.Update(tea.MouseClickMsg{X: x, Y: 1, Button: tea.MouseLeft})
				m.Update(tea.MouseMotionMsg{X: x - 6, Y: 1, Button: tea.MouseLeft})
				if settling {
					m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
				}
				scheduler.step = animation.ShortDuration / 3
				queued = sidebarGuideTestTick(t, m, queued)
				require.NotEqual(t, m.View().Content, m.viewCache.Content)
				elapsed := m.ar.Now()
				tc.lose(m)
				assert.False(t, chat.IsResizingSidebar(p))
				assert.False(t, m.ar.HasActive())
				assert.Equal(t, m.viewCache.Content, m.View().Content)
				assert.Nil(t, sidebarGuideTestTick(t, m, queued), "a queued tick cannot restart the guide")
				assert.Equal(t, elapsed, m.ar.Now())
				m.Update(tea.FocusMsg{})
				m.Update(tea.MouseReleaseMsg{X: x - 6, Y: 1, Button: tea.MouseLeft})
				assert.Equal(t, initial, p.GetSidebarSettings())
				assert.Equal(t, m.viewCache.Content, m.View().Content)
			})
		}
	}
}
