package tui

import (
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

func TestComposerGripFollowsDragFromPaddedEdge(t *testing.T) {
	for _, chrome := range []string{"plain", "tabs and attachment"} {
		t.Run(chrome, func(t *testing.T) {
			m, _, _ := frozenClockRoot(t, 160, 40)
			m.composerGrip = resizegrip.Reveal{} // Snap to targets; timed fades are tested in resizegrip.
			if chrome != "plain" {
				m.tabBar.SetTabs([]messages.TabInfo{{SessionID: "profile", Title: "One"}, {SessionID: "second", Title: "Two"}}, 0)
				m.Update(tea.WindowSizeMsg{Width: m.width, Height: m.height})
				file := filepath.Join(t.TempDir(), "attachment.txt")
				require.NoError(t, os.WriteFile(file, []byte("attached"), 0o600))
				before := m.contentHeight
				m.Update(messages.InsertFileRefMsg{FilePath: file})
				require.Equal(t, 1, m.activeTab.editor.BannerHeight())
				require.Equal(t, before-1, m.contentHeight, "attachment adds a row without taking an editable row")
			}
			textRows := func() int {
				_, editorHeight := m.activeTab.editor.GetSize()
				return editorHeight - styles.EditorStyle.GetVerticalFrameSize() - m.activeTab.editor.BannerHeight()
			}
			assert.Equal(t, 3, textRows(), "default matches upstream's editable height")
			y := m.contentHeight
			start := (m.width - resizeHandleWidth) / 2
			segment := strings.Repeat("─", resizeHandleWidth)
			blank := strings.Repeat(" ", m.width)
			shown := blank[:start] + segment + blank[start+resizeHandleWidth:]
			assert.Equal(t, blank, m.renderResizeHandle(m.width))
			m.Update(tea.MouseMotionMsg{X: start - 1, Y: y})
			edge := m.renderResizeHandle(m.width)
			assert.Equal(t, shown, ansi.Strip(edge), "only the compact segment is revealed")
			m.Update(tea.MouseMotionMsg{X: start, Y: y})
			assert.Contains(t, m.renderResizeHandle(m.width), styles.ResizeHandleHoverStyle.Render(segment))

			assert.False(t, m.composerResizeHit(m.width/2, m.editorTop()+styles.EditorStyle.GetPaddingTop()))
			m.Update(tea.MouseClickMsg{X: -1, Y: y, Button: tea.MouseLeft})
			require.False(t, m.isDragging)
			m.Update(tea.MouseClickMsg{X: 0, Y: y, Button: tea.MouseLeft})
			require.True(t, m.isDragging, "the padded edge remains draggable")
			assert.NotEqual(t, edge, m.renderResizeHandle(m.width), "press darkens the grip")
			for _, row := range []int{y, y - 4} {
				m.Update(tea.MouseMotionMsg{X: 0, Y: row, Button: tea.MouseLeft})
				require.Equal(t, row, m.contentHeight, "divider follows the pointer")
				assert.Equal(t, shown, strings.Split(ansi.Strip(m.View().Content), "\n")[row])
			}
			m.Update(tea.MouseMotionMsg{X: 0, Y: m.height - 1, Button: tea.MouseLeft})
			assert.Equal(t, y, m.contentHeight, "manual minimum matches the default")
			assert.Equal(t, 3, textRows(), "drag floor keeps three editable rows")
			m.Update(tea.MouseReleaseMsg{X: -1, Y: y, Button: tea.MouseLeft})
			assert.False(t, m.isDragging)
			assert.Equal(t, blank, m.renderResizeHandle(m.width))
		})
	}
}

func TestComposerGripLosesCapture(t *testing.T) {
	for _, tc := range []struct {
		name string
		lose func(*appModel)
	}{
		{"blur", func(m *appModel) { m.Update(tea.BlurMsg{}) }},
		{"modal", func(m *appModel) { m.updateDialogCmd(dialog.OpenDialogMsg{Model: &stubDialog{id: "resize-occlusion"}}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _, _ := frozenClockRoot(t, 160, 40)
			m.Update(tea.MouseClickMsg{X: m.width / 2, Y: m.contentHeight, Button: tea.MouseLeft})
			require.True(t, m.isDragging)
			tc.lose(m)
			assert.False(t, m.isDragging)
			assert.Equal(t, strings.Repeat(" ", m.width), m.renderResizeHandle(m.width))
		})
	}
}

func TestComposerSeparatorStaysBlankWhilePauseMovesToStatus(t *testing.T) {
	m, _, _ := frozenClockRoot(t, 160, 40)
	m.Update(agentruntime.StreamStarted("profile", "root"))
	require.True(t, m.activeTab.chatPage.IsWorking())
	check := func(want string) {
		t.Helper()
		rows := strings.Split(ansi.Strip(m.View().Content), "\n")
		assert.Empty(t, strings.TrimSpace(rows[m.contentHeight]))
		status := rows[len(rows)-1]
		assert.Contains(t, status, want)
		assert.LessOrEqual(t, lipgloss.Width(status), m.width)
	}
	m.activeTab.sessionState.SetPauseState(service.PausePausing)
	m.Update(tea.WindowSizeMsg{Width: 24, Height: 12})
	check("Pausing…")
	m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	check("/pause to cancel")
	m.Update(agentruntime.StreamStopped("profile", "root", ""))
	require.False(t, m.activeTab.chatPage.IsWorking())
	check("Paused (/pause to resume)")
}

func TestHistorySuggestionFollowsComposerResize(t *testing.T) {
	const width, height = 120, 40
	preview := []string{"history preview", "preview row two", "preview row three", "preview row four", "preview row five", "preview row six"}
	entry := strings.Join(preview, "\n")
	m, _, _ := frozenClockRoot(t, width, height)
	require.NoError(t, m.history.Add(entry))
	baseline := strings.Split(ansi.Strip(m.View().Content), "\n")
	require.Len(t, baseline, height)
	status := baseline[height-1]
	require.NotEmpty(t, strings.TrimSpace(status))
	checkPreview := func(visible int) {
		t.Helper()
		frame := ansi.Strip(m.View().Content)
		rows := strings.Split(frame, "\n")
		require.Len(t, rows, height)
		assert.Equal(t, status, rows[height-1], "status remains on the last terminal row")
		assert.Equal(t, ansi.Strip(m.renderResizeHandle(m.width)), rows[m.contentHeight])
		firstTextRow := m.editorTop() + styles.EditorStyle.GetPaddingTop()
		for i, want := range preview[:visible] {
			assert.Contains(t, rows[firstTextRow+i], want)
		}
		for _, hidden := range preview[visible:] {
			assert.NotContains(t, frame, hidden)
		}
	}

	m.Update(tea.KeyPressMsg{Code: 'h', Text: "h"})
	checkPreview(3)

	x, y := width/2, m.contentHeight
	m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.True(t, m.isDragging)
	m.Update(tea.MouseMotionMsg{X: x, Y: y - 2, Button: tea.MouseLeft})
	checkPreview(5)
	m.Update(tea.MouseMotionMsg{X: x, Y: y, Button: tea.MouseLeft})
	m.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
	checkPreview(3)

	require.Equal(t, "h", m.activeTab.editor.Value(), "the preview isn't editor input")
	m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	assert.Equal(t, entry, m.activeTab.editor.Value())
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

func TestSidebarResizeGuidePreviewsBeforeCommitting(t *testing.T) {
	for _, tc := range []struct {
		dock      messages.SidebarPosition
		direction int
	}{{messages.SidebarLeft, 1}, {messages.SidebarRight, -1}} {
		t.Run(string(tc.dock), func(t *testing.T) {
			m, x, clock := sidebarResizeTestRoot(t, tc.dock)
			p := m.activeTab.chatPage
			initial := p.GetSidebarSettings()
			_, cmd := m.Update(tea.MouseClickMsg{X: x, Y: 1, Button: tea.MouseLeft})
			require.True(t, chat.IsResizingSidebar(p))
			compact := requireSidebarGuide(t, m)
			assert.Equal(t, 3, compact.End-compact.Start)
			_ = m.View()
			base, generation := m.viewCache.Content, p.VisualGeneration()

			previewX := x + 5*tc.direction
			m.Update(tea.MouseMotionMsg{X: previewX, Y: m.height + 1, Button: tea.MouseLeft})
			require.True(t, m.viewCacheValid, "captured motion does not rebuild the root")
			assert.Equal(t, previewX, requireSidebarGuide(t, m).X)
			clock.step = animation.ShortDuration / 4
			cmd = sidebarGuideTestTick(t, m, cmd)
			growing := requireSidebarGuide(t, m)
			assert.Less(t, growing.Start, compact.Start)
			assert.Greater(t, growing.End, compact.End)
			clock.step = animation.ShortDuration
			require.Nil(t, sidebarGuideTestTick(t, m, cmd))
			assert.False(t, m.ar.HasActive(), "a fully extended guide stops ticking")
			require.True(t, m.viewCacheValid, "guide-only ticks do not rebuild the root")
			assert.Equal(t, base, m.viewCache.Content)
			assert.Equal(t, generation, p.VisualGeneration(), "preview doesn't relayout the transcript")
			assert.Equal(t, initial, p.GetSidebarSettings())
			shown := requireSidebarGuide(t, m)
			assert.Equal(t, chat.SidebarGuide{X: previewX, Start: 0, End: m.contentHeight, Opacity: 1}, shown)
			rows := strings.Split(ansi.Strip(m.View().Content), "\n")
			for y := range m.contentHeight {
				assert.Equal(t, "│", ansi.Cut(rows[y], previewX, previewX+1), "guide row %d", y)
			}
			assert.Equal(t, strings.Split(ansi.Strip(base), "\n")[m.contentHeight:], rows[m.contentHeight:])
			m.Update(agentruntime.UserMessage("LIVE-RESIZE-PREVIEW-MARKER", "profile", nil))
			assert.Contains(t, ansi.Strip(m.View().Content), "-RESIZE-PREVIEW-MARKER", "content stays live under preview")

			drop := tea.MouseReleaseMsg{X: x + 7*tc.direction, Y: -1, Button: tea.MouseLeft}
			_, cmd = m.Update(drop)
			require.False(t, chat.IsResizingSidebar(p))
			assert.Equal(t, initial.PreferredWidth+7, p.GetSidebarSettings().PreferredWidth)
			assert.Equal(t, shown, requireSidebarGuide(t, m), "retraction starts at the last visible preview")
			_ = m.View()
			assert.Equal(t, drop.X, renderedSidebarDivider(t, m.viewCache.Content, tc.dock))
			generation = p.VisualGeneration()
			m.Update(tea.MouseReleaseMsg{X: x, Y: -1, Button: tea.MouseLeft})
			assert.Equal(t, initial.PreferredWidth+7, p.GetSidebarSettings().PreferredWidth, "a second release cannot commit")
			assert.Equal(t, generation, p.VisualGeneration())
			require.Nil(t, sidebarGuideTestTick(t, m, cmd))
			assert.Equal(t, m.viewCache.Content, m.View().Content)
		})
	}
}

func TestSidebarResizeGuideEndsWithoutCommit(t *testing.T) {
	for _, tc := range []struct {
		name      string
		interrupt func(*appModel, int, int) tea.Cmd
		retract   bool
	}{
		{"escape", func(m *appModel, _, _ int) tea.Cmd {
			_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
			return cmd
		}, true},
		{"lost button", func(m *appModel, x, y int) tea.Cmd { _, cmd := m.Update(tea.MouseMotionMsg{X: x, Y: y}); return cmd }, true},
		{"blur", func(m *appModel, _, _ int) tea.Cmd { m.Update(tea.BlurMsg{}); return nil }, false},
		{"hidden", func(m *appModel, _, _ int) tea.Cmd { return m.setPaneHidden(true) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, x, clock := sidebarResizeTestRoot(t, messages.SidebarRight)
			p, y := m.activeTab.chatPage, m.contentHeight/2
			initial := p.GetSidebarSettings()
			_, cmd := m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
			m.Update(tea.MouseMotionMsg{X: x - 9, Y: y, Button: tea.MouseLeft})
			clock.step = animation.ShortDuration / 2
			cmd = sidebarGuideTestTick(t, m, cmd)
			preview := requireSidebarGuide(t, m)
			require.Equal(t, x-9, preview.X)
			require.NotEqual(t, m.View().Content, m.viewCache.Content)
			assert.Nil(t, tc.interrupt(m, x, y), "no extra tick chain on interruption")
			require.False(t, chat.IsResizingSidebar(p))
			if tc.retract {
				assert.Equal(t, preview, requireSidebarGuide(t, m), "retraction starts without a jump")
				cmd = sidebarGuideTestTick(t, m, cmd)
				mid := requireSidebarGuide(t, m)
				assert.Greater(t, mid.X, preview.X, "returns toward the original divider")
				assert.Less(t, mid.End-mid.Start, preview.End-preview.Start)
				assert.Positive(t, mid.Opacity)
				assert.Less(t, mid.Opacity, preview.Opacity)
			}
			require.Nil(t, sidebarGuideTestTick(t, m, cmd), "a queued tick cannot restart a cleared guide")
			assert.False(t, m.ar.HasActive())
			_, visible := chat.SidebarResizeGuide(p)
			assert.False(t, visible)
			m.Update(tea.FocusMsg{})
			m.Update(tea.MouseReleaseMsg{X: x - 9, Y: -1, Button: tea.MouseLeft})
			assert.Equal(t, initial, p.GetSidebarSettings(), "a stale release cannot commit")
			assert.Equal(t, m.viewCache.Content, m.View().Content)
		})
	}
}

func TestSidebarGripFadesWhenPointerLeavesChat(t *testing.T) {
	m, x, _ := sidebarResizeTestRoot(t, messages.SidebarRight)
	rest := m.View().Content
	y := m.contentHeight / 2
	_, cmd := m.Update(tea.MouseMotionMsg{X: x, Y: y})
	require.True(t, m.ar.HasActive())
	require.Nil(t, sidebarGuideTestTick(t, m, cmd))
	assert.Contains(t, m.View().Content, styles.ResizeHandleHoverStyle.Render("│"))
	_, cmd = m.Update(tea.MouseMotionMsg{X: x, Y: m.height - 1})
	require.True(t, m.ar.HasActive())
	require.Nil(t, sidebarGuideTestTick(t, m, cmd))
	assert.Equal(t, rest, m.View().Content, "fade needs no second motion inside chat")
}
