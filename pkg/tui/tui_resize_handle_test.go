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
	"github.com/docker/docker-agent/pkg/userconfig"
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
			m.showTopMargin = true
			m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
			m.composerGrip = resizegrip.Reveal{} // Snap to targets; timed fades are tested in resizegrip.
			if chrome != "plain" {
				m.tabBar.SetTabs([]messages.TabInfo{{SessionID: "profile", Title: "One"}, {SessionID: "second", Title: "Two"}}, 0)
				m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
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
			y := m.topMargin() + m.contentHeight
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
				require.Equal(t, row, m.topMargin()+m.contentHeight, "divider follows the pointer")
				assert.Equal(t, shown, strings.Split(ansi.Strip(m.View().Content), "\n")[row])
			}
			m.Update(tea.MouseMotionMsg{X: 0, Y: m.height - 1, Button: tea.MouseLeft})
			assert.Equal(t, y, m.topMargin()+m.contentHeight, "manual minimum matches the default")
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
			m.Update(tea.MouseClickMsg{X: m.width / 2, Y: m.topMargin() + m.contentHeight, Button: tea.MouseLeft})
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
		assert.Empty(t, strings.TrimSpace(rows[m.topMargin()+m.contentHeight]))
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
		assert.Equal(t, ansi.Strip(m.renderResizeHandle(m.width)), rows[m.topMargin()+m.contentHeight])
		firstTextRow := m.topMargin() + m.editorTop() + styles.EditorStyle.GetPaddingTop()
		for i, want := range preview[:visible] {
			assert.Contains(t, rows[firstTextRow+i], want)
		}
		for _, hidden := range preview[visible:] {
			assert.NotContains(t, frame, hidden)
		}
	}

	m.Update(tea.KeyPressMsg{Code: 'h', Text: "h"})
	checkPreview(3)

	x, y := width/2, m.topMargin()+m.contentHeight
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

func sidebarResizeTestRoot(t *testing.T, position messages.SidebarPosition) (*appModel, int) {
	t.Helper()
	scheduler := &rootImmediateScheduler{now: time.Unix(1, 0)}
	ar := animation.NewRuntimeWithScheduler(scheduler)
	m, _, _ := harnessRoot(t, 160, 40, ar)
	chat.Cleanup(m.activeTab.chatPage)
	p := chat.New(m.ar, t.Context(), m.application, m.activeTab.sessionState,
		chat.WithShowBanner(false), chat.WithLayoutSettings(messages.LayoutSettings{SidebarPosition: position}))
	m.activeTab.chatPage = p
	t.Cleanup(func() { chat.Cleanup(p) })
	p.SetSidebarSettings(chat.SidebarSettings{PreferredWidth: 45})
	m.showTopMargin = true
	m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	return m, renderedSidebarDivider(t, m.View().Content, position)
}

func renderedSidebarDivider(t *testing.T, frame string, position messages.SidebarPosition) int {
	t.Helper()
	glyph := "»"
	if position == messages.SidebarLeft {
		glyph = "«"
	}
	before, _, ok := strings.Cut(ansi.Strip(frame), glyph)
	require.True(t, ok, "sidebar toggle not drawn")
	return lipgloss.Width(before[strings.LastIndexByte(before, '\n')+1:])
}

func settleSidebarGrip(t *testing.T, m *appModel, cmd tea.Cmd) {
	t.Helper()
	for m.ar.HasActive() {
		msgs := collectMsgs(cmd)
		require.Equal(t, 1, countOfType[animation.TickMsg](msgs), "grip uses exactly one shared tick chain")
		tick, _ := firstOfType[animation.TickMsg](msgs)
		_, cmd = m.Update(tick)
	}
}

func TestSidebarDividerMovesWhileDragging(t *testing.T) {
	for _, dock := range []messages.SidebarPosition{messages.SidebarLeft, messages.SidebarRight} {
		t.Run(string(dock), func(t *testing.T) {
			m, x := sidebarResizeTestRoot(t, dock)
			p := m.activeTab.chatPage
			initial := p.GetSidebarSettings()
			for _, cancel := range []bool{true, false} {
				m.Update(tea.MouseClickMsg{X: x, Y: 2, Button: tea.MouseLeft})
				for _, delta := range []int{4, 7, 3} {
					m.Update(tea.MouseMotionMsg{X: x + delta, Y: m.height + 1, Button: tea.MouseLeft})
					require.Equal(t, x+delta, renderedSidebarDivider(t, m.View().Content, dock))
					require.Equal(t, initial, p.GetSidebarSettings())
				}
				if cancel {
					m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
					require.Equal(t, x, renderedSidebarDivider(t, m.View().Content, dock))
				} else {
					m.Update(tea.MouseReleaseMsg{X: x + 5, Y: -1, Button: tea.MouseLeft})
					require.Equal(t, x+5, renderedSidebarDivider(t, m.View().Content, dock))
					require.NotEqual(t, initial, p.GetSidebarSettings())
				}
				require.False(t, chat.IsResizingSidebar(p))
			}
		})
	}
}

func TestSidebarGripFadesWhenPointerLeavesChat(t *testing.T) {
	m, x := sidebarResizeTestRoot(t, messages.SidebarRight)
	rest := m.View().Content
	y := m.contentHeight / 2
	_, cmd := m.Update(tea.MouseMotionMsg{X: x, Y: y})
	require.True(t, m.ar.HasActive())
	settleSidebarGrip(t, m, cmd)
	assert.Contains(t, m.View().Content, styles.ResizeHandleHoverStyle.Render("│"))
	_, cmd = m.Update(tea.MouseMotionMsg{X: x, Y: m.height - 1})
	require.True(t, m.ar.HasActive())
	settleSidebarGrip(t, m, cmd)
	assert.Equal(t, rest, m.View().Content, "fade needs no second motion inside chat")
}

func TestTopMarginFollowsTerminalHeight(t *testing.T) {
	m, x := sidebarResizeTestRoot(t, messages.SidebarRight)
	m.hideSidebar = false
	m.Update(agentruntime.SessionTitle("profile", strings.Repeat("a", 2080)))
	for _, collapsed := range []bool{false, true} {
		m.Update(tea.WindowSizeMsg{Width: 160, Height: 24})
		compact := m.View().Content
		m.Update(tea.WindowSizeMsg{Width: 160, Height: 25})
		require.Equal(t, strings.Repeat(" ", 160)+"\n"+compact, m.View().Content, "collapsed=%v", collapsed)
		if !collapsed {
			m.Update(tea.MouseClickMsg{X: x, Y: 0, Button: tea.MouseLeft})
			require.False(t, m.activeTab.chatPage.GetSidebarSettings().Collapsed)
			_, cmd := m.Update(tea.MouseClickMsg{X: x, Y: 1, Button: tea.MouseLeft})
			for _, msg := range collectMsgs(cmd) {
				m.Update(msg)
			}
			require.True(t, m.activeTab.chatPage.GetSidebarSettings().Collapsed)
		}
	}
}

func TestTopMarginAppliesOnSaveAndSurvivesRestart(t *testing.T) {
	setupSettingsConfigTest(t)

	const width, height = 160, 25
	m, _, _ := frozenClockRoot(t, width, height)
	row := strings.Repeat(" ", width) + "\n"
	check := func(m *appModel, visible bool) {
		t.Helper()
		frame := m.View().Content
		assert.Equal(t, visible, strings.HasPrefix(frame, row))
		assert.Equal(t, height, lipgloss.Height(frame))
		assert.Equal(t, height, m.height+m.topMargin())
	}
	check(m, false)

	prefs := defaultTestPreferences()
	prefs.TopMargin = true
	m.Update(messages.ApplySettingsMsg{Preferences: prefs})
	check(m, true)
	assert.True(t, userconfig.Get().GetTopMargin())

	// Settings changes elsewhere must preserve the saved margin.
	require.NoError(t, saveSettingsToUserConfig(prefs.Layout, prefs.SendMode))
	restarted, _, _ := frozenClockRoot(t, width, height)
	check(restarted, true)

	prefs.TopMargin = false
	restarted.Update(messages.ApplySettingsMsg{Preferences: prefs})
	check(restarted, false)
	require.False(t, userconfig.Get().GetTopMargin())

	// A second restart checks that disabling the setting also persists.
	restarted, _, _ = frozenClockRoot(t, width, height)
	check(restarted, false)
}

func TestLeftDividerRepaintsOnCaptureLossAndResize(t *testing.T) {
	m, x := sidebarResizeTestRoot(t, messages.SidebarLeft)
	m.Update(tea.WindowSizeMsg{Width: 160, Height: 16})
	y := m.topMargin() + 1
	edge := func() string { return ansi.Cut(strings.Split(ansi.Strip(m.View().Content), "\n")[y], x, x+1) }
	for near := x; near >= x-6 && edge() != " "; near-- {
		_, cmd := m.Update(tea.MouseMotionMsg{X: near, Y: y})
		settleSidebarGrip(t, m, cmd)
	}
	require.Equal(t, " ", edge(), "revealed scrollbar hides divider")
	m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.True(t, chat.IsResizingSidebar(m.activeTab.chatPage))
	require.Equal(t, "│", edge())
	m.Update(tea.MouseMotionMsg{X: x, Y: y, Button: tea.MouseNone})
	require.False(t, chat.IsResizingSidebar(m.activeTab.chatPage))
	require.Equal(t, " ", edge(), "first frame after capture loss")
	m.Update(tea.WindowSizeMsg{Width: 160, Height: 100})
	y = m.topMargin() + 1
	require.Equal(t, "│", edge(), "first frame after sidebar content fits")
}
