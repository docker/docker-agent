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
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/animation/animationtest"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const composerDraft = "first\nsecond\nthird\nfourth\nfifth\nsixth\nlast"

func TestComposerGrowsForTypedRows(t *testing.T) {
	root, _, _ := frozenClockRoot(t, 80, 30)
	_, initial := root.activeTab.editor.GetSize()
	initialContent := root.contentHeight
	for _, r := range composerDraft {
		if r == '\n' {
			root.Update(tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl})
		} else {
			root.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
		}
	}
	require.Equal(t, composerDraft, root.activeTab.editor.Value())
	_, grown := root.activeTab.editor.GetSize()
	require.Equal(t, initial+6, grown)
	require.Equal(t, initialContent-6, root.contentHeight)
	view := ansi.Strip(root.activeTab.editor.View())
	require.Contains(t, view, "first")
	require.Contains(t, view, "last")
}

func TestComposerShrinkHoldsThenEasesActualViewport(t *testing.T) {
	scheduler, ar := animationtest.NewClock(time.Unix(1, 0))
	root, _, _ := harnessRoot(t, 80, 30, ar)
	root.Update(messages.RestorePendingMessagesMsg{Content: composerDraft})
	before := root.View().Content
	root.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Empty(t, root.activeTab.editor.Value())
	shrinking := root.View().Content
	require.NotEqual(t, before, shrinking)
	assertComposerMotion(t, root, scheduler, 7, composerMinRows, editorShrinkDelay)
	require.NotEqual(t, shrinking, root.View().Content)

	// Regrowing mid-shrink cancels the pending shrink immediately.
	root.Update(messages.RestorePendingMessagesMsg{Content: composerDraft})
	root.Update(messages.RestorePendingMessagesMsg{Content: "short"})
	require.True(t, root.composer.motion.Running())
	root.Update(messages.RestorePendingMessagesMsg{Content: composerDraft + "\nmore"})
	require.False(t, root.composer.motion.Running())
	_, height := root.activeTab.editor.GetSize()
	require.Equal(t, initialComposerHeight()+7, height)
}

func TestComposerGripPinsHeightAcrossEditsAndTerminalClamps(t *testing.T) {
	root, _, _ := frozenClockRoot(t, 80, 30)
	root.Update(messages.RestorePendingMessagesMsg{Content: composerDraft})
	root.Update(messages.RestorePendingMessagesMsg{Content: "short"})
	require.True(t, root.composer.motion.Running())

	// Move up two rows using the upstream pointer path and release. The
	// chosen allocation should immediately replace the in-flight shrink.
	y := root.contentHeight
	root.Update(tea.MouseClickMsg{X: 40, Y: y, Button: tea.MouseLeft})
	root.Update(tea.MouseMotionMsg{X: 40, Y: y - 2, Button: tea.MouseLeft})
	root.Update(tea.MouseReleaseMsg{X: 40, Y: y - 2, Button: tea.MouseLeft})
	require.False(t, root.composer.motion.Running())
	_, pinned := root.activeTab.editor.GetSize()
	require.Equal(t, initialComposerHeight()+8, pinned)
	require.Equal(t, y-2, root.contentHeight)

	root.Update(messages.RestorePendingMessagesMsg{Content: strings.Repeat("long draft\n", 15)})
	_, height := root.activeTab.editor.GetSize()
	require.Equal(t, pinned, height)
	root.Update(tea.WindowSizeMsg{Width: 80, Height: 6})
	_, height = root.activeTab.editor.GetSize()
	require.Equal(t, 1+styles.EditorStyle.GetVerticalFrameSize(), height)
	require.Equal(t, 1, root.contentHeight)
	require.Equal(t, 6, lipgloss.Height(root.View().Content))
	root.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	_, height = root.activeTab.editor.GetSize()
	require.Equal(t, pinned, height)
}

func TestComposerBudgetsSeparateAttachmentRows(t *testing.T) {
	root, _, _ := frozenClockRoot(t, 200, 30)
	name := filepath.Join(t.TempDir(), "file.txt")
	require.NoError(t, os.WriteFile(name, []byte("attachment"), 0o600))
	root.Update(messages.RestorePendingMessagesMsg{Content: composerDraft + "\n"})
	_, before := root.activeTab.editor.GetSize()
	root.Update(messages.InsertFileRefMsg{FilePath: name})
	require.Equal(t, 3, root.activeTab.editor.BannerHeight())
	_, after := root.activeTab.editor.GetSize()
	require.GreaterOrEqual(t, after, before, "attachment rows do not consume the editable viewport")
	view := ansi.Strip(root.activeTab.editor.View())
	require.Contains(t, ansi.Strip(root.composerView()), filepath.Base(name))
	require.Contains(t, view, "first", "banner must not take an editable row")
	require.Contains(t, view, "last")
	require.Equal(t, regionEditor, root.hitTestRegion(root.composerDock().editorTop+after-1))

	root.Update(tea.WindowSizeMsg{Width: 18, Height: 30})
	width, _ := root.activeTab.editor.GetSize()
	require.Equal(t, 18, width)
	require.LessOrEqual(t, lipgloss.Width(root.activeTab.editor.View()), width)
}

func initialComposerHeight() int {
	return composerMinRows + styles.EditorStyle.GetVerticalFrameSize()
}

func stepClock(t *testing.T, clock *animationtest.Clock, root *appModel) {
	t.Helper()
	root.Update(clock.Step(t))
}

func assertComposerMotion(t *testing.T, root *appModel, scheduler *animationtest.Clock, from, to int, hold time.Duration) {
	t.Helper()
	require.Equal(t, from, root.composer.rows)
	require.Equal(t, to, root.composer.target)
	require.True(t, root.composer.motion.Running())
	require.Equal(t, hold+350*time.Millisecond, root.composer.motion.Duration())
	start := root.ar.Now()
	prior := from
	intermediate := false
	for range 100 {
		if !root.composer.motion.Running() {
			break
		}
		stepClock(t, scheduler, root)
		rows := root.composer.rows
		if root.ar.Now()-start <= hold {
			require.Equal(t, from, rows)
		}
		if from < to {
			require.GreaterOrEqual(t, rows, prior)
			require.LessOrEqual(t, rows, to)
		} else {
			require.LessOrEqual(t, rows, prior)
			require.GreaterOrEqual(t, rows, to)
		}
		intermediate = intermediate || rows != from && rows != to
		_, allocated := root.activeTab.editor.GetSize()
		require.Equal(t, rows+styles.EditorStyle.GetVerticalFrameSize(), allocated)
		prior = rows
	}
	require.True(t, intermediate, "the transition paints intermediate allocations")
	require.False(t, root.composer.motion.Running())
	require.Equal(t, to, root.composer.rows)
	require.LessOrEqual(t, root.ar.Now()-start, hold+350*time.Millisecond+animation.TickRate)
}

func TestComposerHistoryRecallAnimatesAndTypingInterrupts(t *testing.T) {
	for _, direction := range []struct {
		name string
		key  rune
		from int
	}{{"Up", tea.KeyUp, 2}, {"Down", tea.KeyDown, 0}} {
		t.Run(direction.name, func(t *testing.T) {
			scheduler, ar := animationtest.NewClock(time.Unix(1, 0))
			root, _, _ := harnessRoot(t, 80, 40, ar)
			require.NoError(t, root.history.Load())
			root.history.Messages = []string{"older", composerDraft, "newer"}
			root.history.SetCurrent(direction.from)
			require.Equal(t, 1, root.composer.rows)
			root.Update(tea.KeyPressMsg{Code: direction.key})
			require.Equal(t, composerDraft, root.activeTab.editor.Value())
			assertComposerMotion(t, root, scheduler, 1, 7, 0)
			root.Update(tea.KeyPressMsg{Code: direction.key})
			assertComposerMotion(t, root, scheduler, 7, 1, 500*time.Millisecond)

			root.history.SetCurrent(direction.from)
			root.Update(tea.KeyPressMsg{Code: direction.key})
			stepClock(t, scheduler, root)
			require.Less(t, root.composer.rows, 7)
			root.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
			require.Equal(t, composerDraft+"x", root.activeTab.editor.Value())
			require.Equal(t, 7, root.composer.rows, "typing restores the full allocation even with unchanged row count")
			require.False(t, root.composer.motion.Running())
			root.Update(tea.KeyPressMsg{Code: direction.key})
			require.Equal(t, composerDraft+"x", root.activeTab.editor.Value(), "arrows move the cursor in a typed draft")
		})
	}
}

func TestComposerFocusCollapsePreservesDraftAndManualAllocation(t *testing.T) {
	scheduler, ar := animationtest.NewClock(time.Unix(1, 0))
	root, _, _ := harnessRoot(t, 80, 40, ar)
	root.Update(messages.RestorePendingMessagesMsg{Content: composerDraft})
	root.handleEditorResize(root.contentHeight - 2)
	initialContent := root.contentHeight
	require.Equal(t, 9, root.composer.rows)

	root.Update(messages.RequestFocusMsg{Target: messages.PanelMessages})
	require.Equal(t, PanelContent, root.focusedPanel)
	assertComposerMotion(t, root, scheduler, 9, 1, 0)
	require.Equal(t, initialContent+8, root.contentHeight)
	require.Equal(t, composerDraft, root.activeTab.editor.Value())

	root.Update(tea.MouseClickMsg{X: 8, Y: root.composerDock().editorTop + 1, Button: tea.MouseLeft})
	require.Equal(t, PanelEditor, root.focusedPanel)
	assertComposerMotion(t, root, scheduler, 1, 9, 0)
	require.Equal(t, composerDraft, root.activeTab.editor.Value())
	require.Equal(t, initialContent, root.contentHeight)

	// Dragging the divider while transcript is focused focuses the editor and expands.
	root.Update(messages.RequestFocusMsg{Target: messages.PanelMessages})
	root.composer.motion.Cancel()
	root.composer.rows = 1
	root.handleEditorResize(root.composerDock().separatorTop - 3)
	require.Equal(t, PanelEditor, root.focusedPanel)
	require.Equal(t, 4, root.composer.rows)
}

func TestComposerWrappedDraftAfterTerminalResize(t *testing.T) {
	root, _, _ := frozenClockRoot(t, 160, 24)
	draft := "first " + strings.Repeat("aaaa bbbb cccc dddd ", 6) + "END"
	root.Update(tea.PasteMsg{Content: draft})
	root.Update(tea.WindowSizeMsg{Width: 8, Height: 24})

	rows := strings.Split(root.View().Content, "\n")
	editorWidth, editorHeight := root.activeTab.editor.GetSize()
	require.Equal(t, 8, editorWidth)
	lastEditorRow := root.composerDock().editorTop + editorHeight - 1 - styles.EditorStyle.GetPaddingBottom()
	require.Greater(t, len(rows), lastEditorRow)
	require.Equal(t, regionEditor, root.hitTestRegion(lastEditorRow))
	require.Contains(t, ansi.Strip(rows[lastEditorRow]), "END")

	root.Update(messages.WheelCoalescedMsg{X: 1, Y: lastEditorRow, Delta: -100})
	require.Contains(t, ansi.Strip(root.activeTab.editor.View()), "first")
	require.Equal(t, draft, root.activeTab.editor.Value())
}

func TestSpeechSendResetsManualComposerHeight(t *testing.T) {
	root, _, _ := frozenClockRoot(t, 80, 30)
	root.Update(messages.RestorePendingMessagesMsg{Content: composerDraft})
	root.handleEditorResize(root.composerDock().separatorTop - 2)
	require.NotZero(t, root.composer.manualRows)

	cmd := root.sendSpeechDraft(&speechRun{tab: root.activeTab, editor: root.activeTab.editor})
	require.NotNil(t, cmd)
	require.Empty(t, root.activeTab.editor.Value())
	require.Zero(t, root.composer.manualRows)
	require.True(t, root.composer.motion.Running())
}
