package tui

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/components/completion"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func openHelpWithKey(t *testing.T, m *appModel, msg tea.KeyPressMsg) dialog.OpenDialogMsg {
	t.Helper()
	_, cmd := m.Update(msg)
	open, ok := firstOfType[dialog.OpenDialogMsg](collectMsgs(cmd))
	require.True(t, ok, "Help opens with %s", msg.String())
	require.True(t, dialog.IsHelpDialog(open.Model))
	_, _ = m.Update(open)
	return open
}

func TestStatusBarOnlyAdvertisesHelp(t *testing.T) {
	m := newHelpTestModel(t, WithHideSidebar(), WithVersion("test"))
	for _, flags := range []int{0, 1} {
		_, _ = m.Update(tea.KeyboardEnhancementsMsg{Flags: flags})
		bindings := m.Bindings()
		require.Len(t, bindings, 1)
		require.Equal(t, "help", bindings[0].Help().Desc)
		frame := strings.Split(ansi.Strip(m.View().Content), "\n")
		line := strings.TrimRight(frame[len(frame)-1], " \t")
		require.Contains(t, line, "Ctrl+h help")
		require.True(t, strings.HasSuffix(line, "│ + new tab  docker agent test"), "%q", line)
		for _, hidden := range []string{"Ctrl+c", "Ctrl+k", "Ctrl+j", "Shift+Enter", "Tab switch focus"} {
			require.NotContains(t, line, hidden)
		}
	}
	open := openHelpWithKey(t, m, tea.KeyPressMsg{Code: 'h', Mod: tea.ModCtrl})
	view := ansi.Strip(open.Model.View())
	require.Contains(t, view, "Composer")
	require.Contains(t, view, "shift+enter, ctrl+j")
	require.Contains(t, view, "Application")
	require.Contains(t, view, "ctrl+k")
	_, _ = open.Model.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
	require.Contains(t, ansi.Strip(open.Model.View()), "Transcript")
}

func newHelpTestModel(t *testing.T, opts ...Option) *appModel {
	t.Helper()
	sess := &session.Session{ID: t.Name(), Title: "Help test"}
	a := app.New(t.Context(), stubRuntime{}, sess)
	m := New(t.Context(), nil, a, "", func() {}, opts...).(*appModel)
	t.Cleanup(m.cleanupManagedResources)
	_, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	_ = m.Init()
	return m
}

func TestLeanHelpKeepsKeyRoutingWithoutAnOverlay(t *testing.T) {
	m := newHelpTestModel(t, WithLeanMode())
	before := m.View()
	openHelpWithKey(t, m, tea.KeyPressMsg{Code: 'h', Mod: tea.ModCtrl})
	require.Equal(t, before, m.View())

	_, _ = m.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	require.Empty(t, m.activeTab.editor.Value(), "the open dialog still receives keys before the composer")
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	closeMsg, ok := firstOfType[dialog.CloseDialogMsg](collectMsgs(cmd))
	require.True(t, ok)
	_, _ = m.Update(closeMsg)
	require.False(t, m.dialogMgr.Open())
	_, _ = m.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	require.Equal(t, "a", m.activeTab.editor.Value())
}

func TestHelpGroupsShortcutsByTask(t *testing.T) {
	m := newHelpTestModel(t)
	owners := map[string][]string{}
	for _, section := range m.helpSections() {
		for _, b := range section.Bindings {
			for _, k := range b.Keys() {
				owners[k] = append(owners[k], section.Title)
			}
		}
		if section.Title == "Tabs" {
			require.NotContains(t, strings.ToLower(section.Note), "composer")
			require.Len(t, section.Bindings, 3)
		}
	}
	for group, keys := range map[string][]string{
		"Application":     {"ctrl+k", "ctrl+z", "ctrl+h", "ctrl+c", "ctrl+b", "tab"},
		"Tabs":            {"ctrl+t", "ctrl+n", "ctrl+p"},
		"Agent and model": {"ctrl+1", "ctrl+s", "ctrl+m", "shift+tab", "ctrl+y"},
		"Composer":        {"ctrl+w", "ctrl+r", "ctrl+g", "ctrl+x"},
		"Transcript":      {"ctrl+o", "alt+up"},
	} {
		for _, k := range keys {
			require.Equal(t, []string{group}, owners[k], k)
		}
	}
}

func TestHelpDoesNotBreakHiddenShortcuts(t *testing.T) {
	m, _, _ := wallClockRoot(t, 120, 40)
	tabs := []messages.TabInfo{{SessionID: "profile"}, {SessionID: "other"}}
	m.tabBar.SetTabs(tabs, 0)
	m.focusedPanel = PanelContent
	for _, tc := range []struct {
		code rune
		want tea.Msg
	}{
		{'t', messages.SpawnSessionMsg{}},
		{'w', messages.CloseTabMsg{SessionID: "profile"}},
		{'n', messages.SwitchTabMsg{SessionID: "other"}},
		{'p', messages.SwitchTabMsg{SessionID: "other"}},
		{'y', messages.ToggleYoloMsg{}},
		{'o', messages.ToggleHideToolResultsMsg{}},
	} {
		_, cmd := m.Update(tea.KeyPressMsg{Code: tc.code, Mod: tea.ModCtrl})
		require.Contains(t, collectMsgs(cmd), tc.want)
	}
	_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	require.Equal(t, PanelEditor, m.focusedPanel)
}

func TestHelpOverCompletionPreservesDraftAndPopup(t *testing.T) {
	m, _, _ := wallClockRoot(t, 100, 32)
	m.focusedPanel = PanelEditor
	m.activeTab.editor.SetValue("draft")
	_, _ = m.completions.Update(completion.OpenMsg{Items: []completion.Item{{Label: "candidate", Value: "candidate"}}})
	require.True(t, m.completions.Open())
	openHelpWithKey(t, m, tea.KeyPressMsg{Code: 'h', Mod: tea.ModCtrl})
	view := ansi.Strip(m.View().Content)
	require.Contains(t, view, "Completion popup")
	require.Contains(t, view, "Complete without submitting")
	for _, r := range "dismiss comp" {
		_, _ = m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	view = ansi.Strip(m.dialogMgr.TopDialog().View())
	require.Contains(t, view, "Dismiss completion")
	require.NotContains(t, view, "Complete without submitting")
	require.True(t, m.completions.Open())
	require.Equal(t, "draft", m.activeTab.editor.Value())
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	closeMsg, ok := firstOfType[dialog.CloseDialogMsg](collectMsgs(cmd))
	require.True(t, ok)
	_, _ = m.Update(closeMsg)
	require.True(t, m.completions.Open())
	require.False(t, m.dialogMgr.Open())
}

func TestExitConfirmationAboveHelpAndCompletion(t *testing.T) {
	m, _, _ := wallClockRoot(t, 100, 24)
	item := completion.Item{Label: "candidate", Value: "candidate"}
	_, _ = m.completions.Update(completion.OpenMsg{Items: slices.Repeat([]completion.Item{item}, 10)})
	help := openHelpWithKey(t, m, tea.KeyPressMsg{Code: 'h', Mod: tea.ModCtrl})
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	applyOpenDialogMsgs(t, m, cmd)
	require.True(t, m.dialogMgr.TopIsExitConfirmation())
	require.Contains(t, ansi.Strip(m.View().Content), "Do you want to exit?")

	_, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	closeMsg, ok := firstOfType[dialog.CloseDialogMsg](collectMsgs(cmd))
	require.True(t, ok)
	_, _ = m.Update(closeMsg)
	require.Same(t, help.Model, m.dialogMgr.TopDialog())
	require.True(t, m.completions.Open())
	require.Contains(t, ansi.Strip(m.View().Content), "Completion popup")
}

func TestHelpClickTakesPriorityOverCoveredNotification(t *testing.T) {
	m, _, _ := frozenClockRoot(t, 120, 40)
	// Put the toast's close button behind Help's draggable title.
	m.notification.SetSize(90, 7)
	m.Update(notification.ShowMsg{Text: "Notification behind Help"})
	closeGlyph := notificationCloseGlyph(t, m)
	openHelpWithKey(t, m, tea.KeyPressMsg{Code: 'h', Mod: tea.ModCtrl})
	require.NotContains(t, ansi.Strip(m.View().Content), "✕")

	click := tea.MouseClickMsg{X: closeGlyph.X, Y: closeGlyph.Y, Button: tea.MouseLeft}
	_, cmd := m.Update(click)
	require.False(t, hasMsg[notification.DismissMsg](collectMsgs(cmd)), "covered close button must not dismiss the toast")
	m.Update(tea.MouseMotionMsg{X: click.X, Y: click.Y + 10, Button: tea.MouseLeft})
	m.Update(tea.MouseReleaseMsg{X: click.X, Y: click.Y + 10, Button: tea.MouseLeft})
	require.Equal(t, closeGlyph, notificationCloseGlyph(t, m), "click reached Help's title and let it drag off the toast")

	_, cmd = m.Update(click)
	require.True(t, hasMsg[notification.DismissMsg](collectMsgs(cmd)), "uncovered close button still works with Help open")
}

func TestHelpMotionDoesNotHoverCoveredNotification(t *testing.T) {
	m, _, _ := frozenClockRoot(t, 120, 40)
	m.Update(notification.ShowMsg{Text: "Notification peeking out from behind Help"})
	closeGlyph := notificationCloseGlyph(t, m)
	openHelpWithKey(t, m, tea.KeyPressMsg{Code: 'h', Mod: tea.ModCtrl})
	_ = m.View()
	layer := m.dialogMgr.GetLayers()[0]
	right, y := layer.GetX()+layer.Width(), closeGlyph.Y
	_, _, covered := m.notification.BodyHit(right-1, y)
	require.True(t, covered, "test needs a toast body cell just inside Help")
	require.Less(t, y, layer.GetY()+layer.Height())

	m.Update(tea.MouseMotionMsg{X: right, Y: y})
	_, _, hovered := m.notification.CopyHit(right, y)
	require.True(t, hovered, "motion outside Help can hover the visible part of the toast")

	m.Update(tea.MouseMotionMsg{X: right - 1, Y: y})
	_, _, hovered = m.notification.CopyHit(right-1, y)
	require.False(t, hovered, "motion inside Help must clear hover on the covered toast")
}

func TestHelpInModalLeavesCtrlHToTextInput(t *testing.T) {
	m, _, _ := wallClockRoot(t, 100, 32)
	palette := dialog.NewCommandPaletteDialog(nil)
	_, _ = m.Update(dialog.OpenDialogMsg{Model: palette})
	_, _ = m.Update(tea.KeyPressMsg{Code: 'z', Text: "z"})
	before := palette.View()
	_, _ = m.Update(tea.KeyPressMsg{Code: 'h', Mod: tea.ModCtrl})
	require.Same(t, palette, m.dialogMgr.TopDialog())
	require.NotEqual(t, before, palette.View(), "Ctrl+h deletes from palette filter")
	before = palette.View()
	open := openHelpWithKey(t, m, tea.KeyPressMsg{Code: tea.KeyF1})
	require.Contains(t, ansi.Strip(open.Model.View()), "Command palette")
	_, _ = m.Update(tea.PasteMsg{Content: "run selectedx"})
	require.Contains(t, ansi.Strip(open.Model.View()), "No matching shortcuts.")
	_, _ = m.Update(tea.KeyPressMsg{Code: 'h', Mod: tea.ModCtrl})
	require.Contains(t, ansi.Strip(open.Model.View()), "Run selected command")
	require.Equal(t, before, palette.View(), "search input stays in Help")
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyF1})
	require.Nil(t, cmd, "F1 must not stack Help on itself")
	_, _ = m.Update(dialog.CloseDialogMsg{})
	require.Same(t, palette, m.dialogMgr.TopDialog())
}

func TestHelpUsesConfiguredBindingsAndOmitsShadowedAliases(t *testing.T) {
	m, _, _ := wallClockRoot(t, 120, 40)
	cfg := &userconfig.Config{Settings: &userconfig.Settings{Keybindings: []userconfig.Keybinding{
		{Action: "help", Keys: []string{"f8", "f9"}},
		{Action: "quit", Keys: []string{"ctrl+q"}},
		{Action: "editor_send", Keys: []string{"ctrl+d"}},
		{Action: "editor_newline", Keys: []string{"alt+enter", "ctrl+l"}},
	}}}
	require.NoError(t, cfg.Save())
	core.ResetKeys()
	t.Cleanup(core.ResetKeys)
	m.focusedPanel = PanelEditor
	m.statusBar.InvalidateCache()
	require.Contains(t, ansi.Strip(m.statusBar.View()), "F8 help")
	require.NotContains(t, ansi.Strip(m.statusBar.View()), "Ctrl+h help")
	sections := m.helpSections()
	var send, newline []string
	for _, b := range sections[0].Bindings {
		switch b.Help().Desc {
		case "send message":
			send = b.Keys()
		case "Insert newline":
			newline = b.Keys()
		}
	}
	require.Equal(t, []string{"ctrl+d"}, send)
	require.Equal(t, []string{"ctrl+l"}, newline)
	open := openHelpWithKey(t, m, tea.KeyPressMsg{Code: tea.KeyF8})
	require.Contains(t, ansi.Strip(open.Model.View()), "f8, f9")
	_, _ = open.Model.Update(tea.PasteMsg{Content: "exit"})
	require.Regexp(t, `ctrl\+q +Exit TUI *│`, ansi.Strip(open.Model.View()))
}

func TestHelpAlignsShortcutDescriptionsAcrossWrappedRows(t *testing.T) {
	aliases := regexp.MustCompile(`ctrl\+[1-9]`)
	for _, width := range []int{42, 120} {
		t.Run(strconv.Itoa(width), func(t *testing.T) {
			m := newHelpTestModel(t)
			_, _ = m.Update(tea.WindowSizeMsg{Width: width, Height: 50})
			open := openHelpWithKey(t, m, tea.KeyPressMsg{Code: 'h', Mod: tea.ModCtrl})
			_ = open.Model.View()
			_, _ = open.Model.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
			view := open.Model.View()
			// Keep the wrapped row away from the scroll-edge fade when checking its style.
			for !strings.Contains(ansi.Strip(view), "Agent and model") {
				previous := view
				_, _ = open.Model.Update(tea.KeyPressMsg{Code: tea.KeyUp})
				view = open.Model.View()
				require.NotEqual(t, previous, view, "agent shortcuts not found")
			}
			plain := ansi.Strip(view)
			require.Equal(t, []string{"ctrl+1", "ctrl+2", "ctrl+3", "ctrl+4", "ctrl+5", "ctrl+6", "ctrl+7", "ctrl+8", "ctrl+9"}, aliases.FindAllString(plain, -1), "%s", plain)
			column := func(text string) int {
				t.Helper()
				for line := range strings.SplitSeq(plain, "\n") {
					if i := strings.Index(line, text); i >= 0 {
						return ansi.StringWidth(line[:i])
					}
				}
				t.Fatalf("%q not visible in:\n%s", text, plain)
				return 0
			}
			require.Regexp(t, `ctrl\+1,[^\n]*Switch directly`, plain)
			for line := range strings.SplitSeq(plain, "\n") {
				require.LessOrEqual(t, ansi.StringWidth(line), width)
			}
			descColumn := column("Cycle thinking")
			require.Equal(t, descColumn, column("Switch directly"))
			if width == 42 {
				require.Equal(t, descColumn, column("to an agent by"))
				require.Equal(t, descColumn, column("level (when"))
				require.Contains(t, view, styles.SecondaryStyle.Render("to an agent by"))
			}
			require.Contains(t, view, styles.BoldStyle.Foreground(styles.Accent).Italic(true).Render("Transcript"))
			require.Contains(t, view, styles.HighlightWhiteStyle.Render("shift+tab"))
			require.Contains(t, plain, "↑↓ scroll  Esc close")
		})
	}
}

func TestHelpStylesWrappedPickerNote(t *testing.T) {
	m := newHelpTestModel(t)
	_, _ = m.Update(tea.WindowSizeMsg{Width: 42, Height: 50})
	_, _ = m.Update(dialog.OpenDialogMsg{Model: dialog.NewCommandPaletteDialog(nil)})
	open := openHelpWithKey(t, m, tea.KeyPressMsg{Code: tea.KeyF1})
	view := open.Model.View()
	require.Contains(t, view, styles.BoldStyle.Foreground(styles.Accent).Italic(true).Render("Command palette"))
	require.Contains(t, view, styles.DialogHelpStyle.Render("Type to filter; Ctrl+h or"))
	require.Contains(t, view, styles.DialogHelpStyle.Render("Backspace deletes text."))
	require.Contains(t, view, styles.SecondaryStyle.Render("Run selected"))
	require.Contains(t, view, styles.SecondaryStyle.Render("command"))
}
