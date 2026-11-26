package tui

import (
	"context"
	"testing"
	"testing/synctest"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/history"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/commands"
	"github.com/docker/docker-agent/pkg/tui/components/editor"
	"github.com/docker/docker-agent/pkg/tui/core/input"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestDialogStackKeepsOnlyTopCursorActive(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		root, composer := newComposerRoot(t, "composer draft")
		root.buildCommandCategories = func(context.Context, tea.Model) []commands.Category { return nil }
		root.dialogMgr.Update(tea.WindowSizeMsg{Width: 100, Height: 35})
		update := updater(root)
		esc := tea.KeyPressMsg{Code: tea.KeyEscape}

		composerTick := beginComposerBlink(root)
		focusedComposer := composer.View()
		openPalette := update(tea.KeyPressMsg{Code: 'k', Mod: tea.ModCtrl})
		paletteTick := update(openPalette())
		palette := root.dialogMgr.TopDialog()
		require.True(t, dialog.IsCommandPalette(palette))
		coveredComposer := composer.View()
		require.NotEqual(t, focusedComposer, coveredComposer, "opening Commands immediately hides the composer cursor")
		require.Contains(t, ansi.Strip(coveredComposer), "composer draft")

		visiblePalette := palette.View()
		paletteTick = update(cursorTick(t, paletteTick))
		require.NotEqual(t, visiblePalette, palette.View(), "Commands cursor blinks")
		paletteTick = update(cursorTick(t, paletteTick))
		require.Equal(t, visiblePalette, palette.View())
		require.Nil(t, update(cursorTick(t, composerTick)), "the covered composer must not schedule another tick")
		require.Equal(t, coveredComposer, composer.View())
		update(tea.KeyPressMsg{Code: 'x', Text: "x"})
		visiblePalette = palette.View()

		top := dialog.NewHelpDialog(nil)
		topTick := update(dialog.OpenDialogMsg{Model: top})
		coveredPalette := palette.View()
		require.NotEqual(t, visiblePalette, coveredPalette, "stacking hides the previous dialog cursor")
		require.Contains(t, ansi.Strip(coveredPalette), "> x")
		require.Nil(t, update(cursorTick(t, paletteTick)), "the covered dialog must not schedule another tick")
		require.Equal(t, coveredPalette, palette.View())
		visibleTop := top.View()
		update(cursorTick(t, topTick))
		require.NotEqual(t, visibleTop, top.View(), "only the Help search cursor blinks")

		paletteTick = update(update(esc)())
		require.Same(t, palette, root.dialogMgr.TopDialog())
		require.Equal(t, visiblePalette, palette.View(), "previous dialog regains its cursor and query")
		update(cursorTick(t, paletteTick))
		require.NotEqual(t, visiblePalette, palette.View(), "the restored dialog cursor blinks again")
		require.Equal(t, coveredComposer, composer.View())

		composerTick = update(update(esc)())
		require.False(t, root.dialogMgr.Open())
		require.Equal(t, focusedComposer, composer.View(), "composer regains its cursor and draft")
		update(cursorTick(t, composerTick))
		require.NotEqual(t, focusedComposer, composer.View(), "the restored composer cursor blinks again")
		update(tea.KeyPressMsg{Code: '!', Text: "!"})
		require.Equal(t, "composer draft!", composer.Value())
	})
}

func TestDialogRestoresHistorySearchCursor(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		root, _ := newTestModel(t)
		root.leanMode = true
		root.focusedPanel = PanelEditor
		hist, err := history.New(t.TempDir())
		require.NoError(t, err)
		require.NoError(t, hist.Add("matched entry"))
		composer := editor.New(hist)
		composer.SetValue("unfinished draft")
		root.activeTab.editor = composer
		_, _ = composer.EnterHistorySearch()
		_, _ = root.Update(tea.KeyPressMsg{Code: 'm', Text: "m"})
		focused := composer.View()
		require.Contains(t, ansi.Strip(focused), "matched entry")

		root.Update(dialog.OpenDialogMsg{Model: dialog.NewExitConfirmationDialog()})
		require.NotEqual(t, focused, composer.View())
		_, resume := root.Update(dialog.CloseAllDialogsMsg{})
		require.Equal(t, focused, composer.View(), "history search rather than the composer textarea regains the cursor")
		for _, msg := range collectMsgs(resume) {
			root.Update(msg)
		}
		require.NotEqual(t, focused, composer.View(), "the restored history cursor blinks again")

		root.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
		require.False(t, composer.IsHistorySearchActive())
		require.Equal(t, "unfinished draft", composer.Value())
	})
}

func TestCursorSuspendsUntilVisibleAndFocused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		pause  []tea.Msg
		resume tea.Msg
	}{
		{"hidden", []tea.Msg{tmuxVisibilityMsg{hidden: true}}, tmuxVisibilityMsg{}},
		{"blurred", []tea.Msg{tea.BlurMsg{}}, tea.FocusMsg{}},
		{"visible but blurred", []tea.Msg{tmuxVisibilityMsg{hidden: true}, tea.BlurMsg{}, tmuxVisibilityMsg{}}, tea.FocusMsg{}},
	} {
		for _, owner := range []string{"composer", "dialog"} {
			t.Run(tc.name+"/"+owner, func(t *testing.T) {
				t.Parallel()
				synctest.Test(t, func(t *testing.T) {
					root, composer := newComposerRoot(t, "composer draft")
					root.ar = animation.NewRuntime()
					t.Cleanup(root.ar.Stop)
					update := updater(root)
					var start tea.Cmd
					view, draft := composer.View, "composer draft"
					if owner == "dialog" {
						palette := dialog.NewCommandPaletteDialog(nil)
						start = update(dialog.OpenDialogMsg{Model: palette})
						update(tea.KeyPressMsg{Code: 'x', Text: "x"})
						view, draft = palette.View, "> x"
					} else {
						start = beginComposerBlink(root)
					}
					oldTick := cursorTick(t, start)
					visible := view()

					for _, msg := range tc.pause {
						update(msg)
					}
					require.False(t, hasMsg[*input.BlinkMsg](collectMsgs(update(oldTick))), "a queued tick must not sustain blinking while suspended")
					require.NotEqual(t, visible, view(), "suspension suppresses cursor rendering")
					require.Contains(t, ansi.Strip(view()), draft)

					resume := update(tc.resume)
					require.Equal(t, visible, view(), "resuming restores the cursor and draft")
					update(cursorTick(t, resume))
					require.NotEqual(t, visible, view(), "the restored cursor blinks again")
				})
			})
		}
	}
}

func TestSwitchTabRestartsInlineEditCursor(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		root := newTabLifecycleModel(t)
		firstID := root.supervisor.ActiveID()
		root.handleSpawnSession("/other")
		secondID := root.supervisor.ActiveID()

		update := updater(root)
		update(messages.SwitchTabMsg{SessionID: firstID})
		page := root.activeTab.chatPage
		root.focusedPanel = PanelContent
		update(runtime.UserMessage("original message", "profile", nil))
		start := update(messages.EditUserMessageMsg{MsgIndex: 0, SessionPosition: 0, OriginalContent: "edit draft"})
		require.True(t, page.IsInlineEditing())
		update(tea.KeyPressMsg{Code: '!', Text: "!"})
		oldTick := cursorTick(t, start)

		update(messages.SwitchTabMsg{SessionID: secondID})
		update(oldTick)
		resume := update(messages.SwitchTabMsg{SessionID: firstID})
		require.Same(t, page, root.activeTab.chatPage)
		require.True(t, page.IsInlineEditing())
		visible := page.View()
		require.Contains(t, ansi.Strip(visible), "edit draft!")
		newTick := cursorTick(t, resume)
		update(newTick)
		require.NotEqual(t, visible, page.View(), "the restored inline edit cursor blinks again")
	})
}

func cursorTick(t *testing.T, cmd tea.Cmd) *input.BlinkMsg {
	t.Helper()
	for _, msg := range collectMsgs(cmd) {
		if tick, ok := msg.(*input.BlinkMsg); ok {
			return tick
		}
	}
	t.Fatal("expected a cursor blink tick")
	return nil
}

func newComposerRoot(t *testing.T, draft string) (*appModel, editor.Editor) {
	t.Helper()
	root, _ := newTestModel(t)
	root.leanMode = true
	root.focusedPanel = PanelEditor
	composer := editor.New(nil)
	composer.SetValue(draft)
	root.activeTab.editor = composer
	return root, composer
}

func updater(root *appModel) func(tea.Msg) tea.Cmd {
	return func(msg tea.Msg) tea.Cmd {
		_, cmd := root.Update(msg)
		return cmd
	}
}

func beginComposerBlink(root *appModel) tea.Cmd {
	_, cmd := root.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	return cmd
}
