package tui

import (
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/plans"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/core/input"
	"github.com/docker/docker-agent/pkg/tui/dialog"
)

func TestFreshDialogInputBlinks(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		open func(*testing.T) dialog.Dialog
	}{
		{"MCP prompt", func(*testing.T) dialog.Dialog {
			return dialog.NewMCPPromptInputDialog("example", tools.PromptInfo{Arguments: []tools.PromptArgument{{Name: "argument", Description: "Type an argument"}}})
		}},
		{"elicitation", func(*testing.T) dialog.Dialog { return dialog.NewElicitationDialog("Question", nil, nil) }},
		{"session browser", func(*testing.T) dialog.Dialog { return dialog.NewSessionBrowserDialog(nil, "") }},
		{"working directory", func(t *testing.T) dialog.Dialog {
			t.Helper()
			return dialog.NewWorkingDirPickerDialog(t.Context(), nil, nil, nil, t.TempDir())
		}},
		{"plan name", func(t *testing.T) dialog.Dialog {
			t.Helper()
			return planInputDialog(t, 'n')
		}},
		{"plan status", func(t *testing.T) dialog.Dialog {
			t.Helper()
			return planInputDialog(t, 's')
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				root, _ := newTestModel(t)
				root.leanMode = true
				d := tc.open(t)
				root.dialogMgr.Update(tea.WindowSizeMsg{Width: 100, Height: 35})
				_, cmd := root.Update(dialog.OpenDialogMsg{Model: d})
				requireInputBlink(t, root, d.View, cmd)
			})
		})
	}
}

func TestComposerCursorBlinksAfterCtrlJ(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		root, composer := newComposerRoot(t, "draf")
		_, typed := root.Update(tea.KeyPressMsg{Code: 't', Text: "t"})
		pending := requireInputBlink(t, root, composer.View, typed)
		_, newline := root.Update(tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl})
		require.Equal(t, "draft\n", composer.Value())
		requireInputBlink(t, root, composer.View, tea.Batch(pending, newline))
	})
}

func TestComposerInputWakesHiddenCursor(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		draft string
		start bool
		input tea.Msg
		want  string
	}{
		{name: "typing", draft: "draf", input: tea.KeyPressMsg{Code: 't', Text: "t"}, want: "draft"},
		{name: "Ctrl+J", draft: "draf", input: tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl}, want: "draf\n"},
		{name: "no-op", draft: "draf", input: tea.KeyPressMsg{Code: tea.KeyRight}, want: "draf"},
		{name: "backspace on empty draft", input: tea.KeyPressMsg{Code: tea.KeyBackspace}},
		{name: "backspace at start of first line", draft: "draf\nsecond line", start: true, input: tea.KeyPressMsg{Code: tea.KeyBackspace}, want: "draf\nsecond line"},
		{name: "empty terminal paste", draft: "draf", input: tea.PasteMsg{}, want: "draf"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				root, composer := newComposerRoot(t, tc.draft)
				caret := input.CursorOf(composer)
				start := beginComposerBlink(root)
				if tc.start {
					root.Update(tea.KeyPressMsg{Code: tea.KeyUp})
					root.Update(tea.KeyPressMsg{Code: tea.KeyHome})
				}
				_, pending := root.Update(cursorTick(t, start))
				require.False(t, caret.VirtualCursor())

				<-time.NewTimer(200 * time.Millisecond).C
				root.Update(tc.input) // Do not execute any command returned for the input.
				require.Equal(t, tc.want, composer.Value())
				require.True(t, caret.VirtualCursor(), "the input immediately wakes the caret")
				woke := time.Now()

				root.viewCacheValid = true
				_, pending = root.Update(cursorTick(t, pending))
				require.Equal(t, 300*time.Millisecond, time.Since(woke))
				require.True(t, caret.VirtualCursor(), "the outstanding tick must honor the new deadline")
				require.True(t, root.viewCacheValid, "a deferred blink does not invalidate the frame")

				<-time.NewTimer(100 * time.Millisecond).C
				root.Update(tea.KeyboardEnhancementsMsg{}) // Forwarded to the editor, but not input.
				root.Update(cursorTick(t, pending))
				require.Equal(t, 500*time.Millisecond, time.Since(woke))
				require.False(t, caret.VirtualCursor(), "background messages do not extend the refreshed deadline")
				require.False(t, root.viewCacheValid)
			})
		})
	}
}

func planInputDialog(t *testing.T, key rune) dialog.Dialog {
	t.Helper()
	browser := dialog.NewPlanBrowserDialog(plans.ListResult{Plans: []plans.Plan{{Scope: plans.ScopeShared, Name: "release", Version: new(1)}}})
	_, cmd := browser.Update(tea.KeyPressMsg{Code: key, Text: string(key)})
	require.NotNil(t, cmd)
	open, ok := cmd().(dialog.OpenDialogMsg)
	require.True(t, ok)
	return open.Model
}

func requireInputBlink(t *testing.T, root *appModel, view func() string, cmd tea.Cmd) tea.Cmd {
	t.Helper()
	visible := view()
	_, pending := root.Update(cursorTick(t, cmd))
	require.NotEqual(t, visible, view(), "the cursor blinks without further input")
	return pending
}
