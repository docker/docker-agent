package tui

import (
	"bytes"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation/animationtest"
	tuiimage "github.com/docker/docker-agent/pkg/tui/image"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
)

func TestComposerAttachmentFocusAndPreview(t *testing.T) {
	root, _, _ := frozenClockRoot(t, 65, 28)
	for _, name := range []string{"report-one-really-wide.txt", "report-two-really-wide.txt", "report-three-really-wide.txt"} {
		path := filepath.Join(t.TempDir(), name)
		require.NoError(t, os.WriteFile(path, []byte("preview text"), 0o600))
		root.Update(messages.InsertFileRefMsg{FilePath: path})
	}
	d := root.composerDock()
	require.Equal(t, regionAttachments, root.hitTestRegion(d.bannerTop+2))
	root.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	require.Equal(t, PanelAttachments, root.focusedPanel)
	root.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Greater(t, root.activeTab.editor.BannerHeight(), d.separatorTop-d.bannerTop)
	root.Update(tea.MouseClickMsg{X: 2, Y: root.composerDock().bannerTop + 2, Button: tea.MouseLeft})
	require.True(t, root.dialogMgr.Open())
	require.Contains(t, ansi.Strip(root.View().Content), "preview text")
}

func TestComposerAttachmentImagePreview(t *testing.T) {
	var src bytes.Buffer
	require.NoError(t, png.Encode(&src, image.NewGray(image.Rect(0, 0, 8, 8))))
	path := filepath.Join(t.TempDir(), "example.png")
	require.NoError(t, os.WriteFile(path, src.Bytes(), 0o600))
	for _, tc := range []struct {
		name               string
		enabled, supported bool
	}{{"shown", true, true}, {"disabled", false, true}, {"unsupported", true, false}} {
		t.Run(tc.name, func(t *testing.T) {
			root, _, _ := frozenClockRoot(t, 100, 40)
			var out bytes.Buffer
			root.imageWriter = tuiimage.NewWriter(&out)
			root.imageWriter.SetEnabled(tc.enabled)
			root.imageWriter.SetSupported(tc.supported)
			root.Update(messages.InsertFileRefMsg{FilePath: path})
			d := root.composerDock()
			root.Update(tea.MouseClickMsg{X: 2, Y: d.bannerTop + 2, Button: tea.MouseLeft})
			v := root.View().Content
			require.Contains(t, v, "8 × 8 pixels")
			_, err := root.imageWriter.Write([]byte(v))
			require.NoError(t, err)
			require.Equal(t, tc.enabled && tc.supported, strings.Contains(out.String(), "\x1b_Ga=p,"))
		})
	}
}

func TestComposerSingleTabRowAndPointer(t *testing.T) {
	root, _, _ := frozenClockRoot(t, 98, 28)
	require.Equal(t, 1, root.supervisor.Count())
	root.hideSidebar = false
	root.activeTab.chatPage = chat.New(root.ar, t.Context(), root.application, root.activeTab.sessionState)
	root.Update(messages.PreviewLayoutMsg{Layout: messages.LayoutSettings{SidebarPosition: messages.SidebarLeft}})
	d := root.composerDock()
	plain := ansi.Strip(strings.Split(root.View().Content, "\n")[d.tabsTop])
	require.Contains(t, plain, "profile")
	require.Equal(t, d.tabOrigin(), strings.Index(plain, "▎"))
	require.Equal(t, regionTabBar, root.hitTestRegion(d.tabsTop))
	require.Equal(t, regionResizeHandle, root.hitTestRegion(d.tabsTop-1))
	require.Equal(t, regionEditor, root.hitTestRegion(d.tabsTop+1))
	beforePlus, _, found := strings.Cut(plain, "+")
	require.True(t, found)
	plus := ansi.StringWidth(beforePlus)
	_, cmd := root.Update(tea.MouseClickMsg{X: plus, Y: d.tabsTop, Button: tea.MouseLeft})
	require.Contains(t, collectMsgs(cmd), tea.Msg(messages.SpawnSessionMsg{}))
	_, cmd = root.Update(tea.MouseReleaseMsg{X: plus, Y: d.tabsTop, Button: tea.MouseLeft})
	require.NotContains(t, collectMsgs(cmd), tea.Msg(messages.SpawnSessionMsg{}))
	root.Update(tea.MouseClickMsg{X: d.tabOrigin(), Y: d.tabsTop, Button: tea.MouseLeft})
	require.False(t, root.tabBar.IsDragging(), "one tab cannot be reordered")

	root.Update(tea.WindowSizeMsg{Width: 98, Height: 4})
	require.Zero(t, root.composer.tabRows)
	require.Equal(t, regionEditor, root.hitTestRegion(root.composerDock().editorTop))
	root.leanMode = true
	root.Update(tea.WindowSizeMsg{Width: 98, Height: 28})
	require.Zero(t, root.composer.tabRows)
	require.NotContains(t, ansi.Strip(root.composerView()), "▎ profile")
}

func TestComposerHideSingleTabSetting(t *testing.T) {
	setupSettingsConfigTest(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	const width, height = 140, 40
	var spy spySpawner
	start := func() *appModel {
		sess := session.New(session.WithTitle("First"))
		m := New(t.Context(), spy.spawn, app.New(t.Context(), stubRuntime{}, sess), "", func() {}, WithDefaultWorkingDir(home)).(*appModel)
		t.Cleanup(m.Shutdown)
		m.Update(tea.WindowSizeMsg{Width: width, Height: height})
		return m
	}
	m := start()
	row := func(y int) string { return strings.Split(ansi.Strip(m.View().Content), "\n")[y] }
	toggle := func(hide bool) {
		t.Helper()
		m.Update(messages.ApplySettingsMsg{Preferences: messages.Preferences{HideSingleTab: hide}})
	}
	click := func(y int, label string) tea.Cmd {
		t.Helper()
		before, _, found := strings.Cut(row(y), label)
		require.True(t, found, "%q not on row %d", label, y)
		_, cmd := m.Update(tea.MouseClickMsg{X: ansi.StringWidth(before), Y: y, Button: tea.MouseLeft})
		return cmd
	}
	openRows := m.contentHeight
	require.Contains(t, row(m.composerDock().tabsTop), "First")
	require.NotContains(t, row(height-1), "+ new tab")
	toggle(true)
	require.Equal(t, openRows+1, m.contentHeight)
	m.Shutdown()
	m = start()
	require.Equal(t, openRows+1, m.contentHeight)
	require.Contains(t, row(height-1), "+ new tab  docker agent")
	click(height-1, "+ new tab")
	require.Len(t, spy.dirs, 1)
	require.Equal(t, 2, m.supervisor.Count())
	require.Equal(t, openRows, m.contentHeight)
	require.NotContains(t, row(height-1), "+ new tab")
	require.Contains(t, row(m.composerDock().tabsTop), "+")
	closeMsg, ok := firstOfType[messages.CloseTabMsg](collectMsgs(click(m.composerDock().tabsTop, "×")))
	require.True(t, ok)
	m.Update(closeMsg)
	require.Equal(t, openRows+1, m.contentHeight)
	require.Contains(t, row(height-1), "+ new tab")
	require.Contains(t, row(m.composerDock().stripTop), "Context")
	// Toggle HideSingleTab while 2 sessions are open: tab bar stays visible,
	// and closing back to 1 session reflects the toggled setting immediately.
	click(height-1, "+ new tab")
	require.Equal(t, 2, m.supervisor.Count())
	toggle(false) // HideSingleTab: true -> false while 2 tabs open
	require.Equal(t, openRows, m.contentHeight)
	require.Contains(t, row(m.composerDock().tabsTop), "+")
	closeMsg, ok = firstOfType[messages.CloseTabMsg](collectMsgs(click(m.composerDock().tabsTop, "×")))
	require.True(t, ok)
	m.Update(closeMsg)
	require.Equal(t, 1, m.supervisor.Count())
	require.Equal(t, openRows, m.contentHeight)
	require.Contains(t, row(m.composerDock().tabsTop), "+")
	require.NotContains(t, row(height-1), "+ new tab")
}

func TestComposerTabHoverRoutesThroughDockAndClock(t *testing.T) {
	scheduler, ar := animationtest.NewClock(time.Unix(1, 0))
	root, _, _ := harnessRoot(t, 96, 28, ar)
	row := func() string { return strings.Split(root.View().Content, "\n")[root.composerDock().tabsTop] }
	idle := row()
	plain := ansi.Strip(idle)
	cell := func(text string) int {
		before, _, found := strings.Cut(plain, text)
		require.True(t, found)
		return ansi.StringWidth(before)
	}
	plus, closeX, y := cell("+"), cell("×"), root.composerDock().tabsTop
	root.Update(tea.MouseMotionMsg{X: plus, Y: y})
	require.Equal(t, idle, row(), "hover starts at the idle colors")
	stepClock(t, scheduler, root)
	require.NotEqual(t, idle, row())
	require.Equal(t, plain, ansi.Strip(row()), "feedback does not alter pointer geometry")
	root.Update(tea.MouseMotionMsg{X: closeX, Y: y})
	stepClock(t, scheduler, root)
	_, cmd := root.Update(tea.MouseClickMsg{X: closeX, Y: y, Button: tea.MouseLeft})
	require.Contains(t, collectMsgs(cmd), tea.Msg(messages.CloseTabMsg{SessionID: "profile"}))
	root.Update(tea.MouseMotionMsg{X: closeX, Y: y + 1})
	for range 25 {
		if !root.ar.HasActive() {
			break
		}
		stepClock(t, scheduler, root)
	}
	require.Equal(t, idle, row())
	require.Zero(t, root.ar.ActiveCount())
	root.Update(tea.MouseMotionMsg{X: closeX, Y: y})
	stepClock(t, scheduler, root)
	require.NotEqual(t, idle, row())
	root.Update(tea.BlurMsg{})
	require.Equal(t, idle, row())
}

func TestCloseLastTabWithNilSpawnerIsNoOp(t *testing.T) {
	m, _, _ := frozenClockRoot(t, 100, 30)
	// Default frozenClockRoot has nil spawner.
	require.Equal(t, 1, m.supervisor.Count())
	m.Update(messages.CloseTabMsg{SessionID: m.supervisor.ActiveID()})
	require.Equal(t, 1, m.supervisor.Count(), "closing last tab with nil spawner must keep the tab alive")
	require.NotNil(t, m.activeTab)
}

func TestAttachmentBannerFocusClearedOnSidebarTitleAndTabSwitch(t *testing.T) {
	root, _, _ := frozenClockRoot(t, 65, 28)
	for _, name := range []string{"report-one-really-wide.txt", "report-two-really-wide.txt", "report-three-really-wide.txt"} {
		path := filepath.Join(t.TempDir(), name)
		require.NoError(t, os.WriteFile(path, []byte("preview text"), 0o600))
		root.Update(messages.InsertFileRefMsg{FilePath: path})
	}
	root.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	require.Equal(t, PanelAttachments, root.focusedPanel)

	root.Update(messages.RequestFocusMsg{Target: messages.PanelSidebarTitle})
	require.Equal(t, PanelContent, root.focusedPanel)
}

func TestBackgroundTokenUsageIgnoresAccountingOnlyAndNilSession(t *testing.T) {
	m, _, _ := frozenClockRoot(t, 100, 30)
	bgTab := &tabModel{}
	m.tabs["bg"] = bgTab

	// Accounting-only update (ContextLength==0 && ContextLimit==0) must not overwrite pendingUsage.
	want := &runtime.TokenUsageEvent{
		SessionID: "bg",
		Usage:     &runtime.Usage{ContextLength: 1000, ContextLimit: 10000},
	}
	bgTab.pendingUsage = want
	m.handleRoutedMsg(messages.RoutedMsg{
		SessionID: "bg",
		Inner: &runtime.TokenUsageEvent{
			SessionID: "bg",
			Usage:     &runtime.Usage{Cost: 0.05},
		},
	})
	require.Equal(t, want, bgTab.pendingUsage)
}

func TestBannerToggleFromContentPanelAnimatesInsteadOfSnapping(t *testing.T) {
	root, _, _ := frozenClockRoot(t, 65, 28)
	// Multi-line draft so composer shrinks when focusing content panel.
	root.activeTab.editor.SetValue("line1\nline2\nline3\nline4 ")
	for _, name := range []string{"report-one-really-wide.txt", "report-two-really-wide.txt", "report-three-really-wide.txt"} {
		path := filepath.Join(t.TempDir(), name)
		require.NoError(t, os.WriteFile(path, []byte("preview text"), 0o600))
		root.Update(messages.InsertFileRefMsg{FilePath: path})
	}
	root.syncComposerHeight()
	root.setPanelFocus(PanelContent)
	root.syncComposerHeightForFocus(true)
	root.composer.motion.Cancel()
	root.composer.rows = 1

	d := root.composerDock()
	root.Update(tea.MouseClickMsg{X: d.width - 2, Y: d.bannerTop + 2, Button: tea.MouseLeft})
	require.Equal(t, PanelAttachments, root.focusedPanel)
	require.True(t, root.composer.motion.Running(), "toggling banner from content panel should animate composer height")
}
