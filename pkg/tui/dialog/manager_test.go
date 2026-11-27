package dialog

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/plans"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

// TestManagerBackgroundDialog verifies that opening a dialog with a non-nil
// OriginatingEvent marks it as a background dialog, while opening one without
// an event leaves the manager in regular ("modal") mode.
func TestManagerBackgroundDialog(t *testing.T) {
	t.Parallel()

	mgr := New().(*manager)

	assert.False(t, mgr.Open(), "manager starts empty")
	assert.False(t, mgr.TopIsBackground(), "empty manager has no background dialog")
	assert.Nil(t, mgr.TopBackgroundEvent())
	assert.Nil(t, mgr.TopDialog(), "empty manager has no top dialog")

	// Open a regular (modal) dialog without an event.
	modal := NewExitConfirmationDialog()
	mgr.handleOpen(OpenDialogMsg{
		Model: modal,
	})
	assert.True(t, mgr.Open())
	assert.False(t, mgr.TopIsBackground(), "dialog opened without OriginatingEvent must NOT be background")
	assert.Nil(t, mgr.TopBackgroundEvent())
	assert.Same(t, modal, mgr.TopDialog(), "TopDialog returns the modal instance")

	// Stack a background dialog (i.e. one carrying an originating event) on top.
	type fakeEvent struct{ id int }
	event := &fakeEvent{id: 42}
	bg := NewElicitationDialog("Pick a value", nil, nil, "")
	mgr.handleOpen(OpenDialogMsg{
		Model:            bg,
		OriginatingEvent: event,
	})
	assert.True(t, mgr.TopIsBackground(), "dialog opened with OriginatingEvent IS background")
	assert.Same(t, event, mgr.TopBackgroundEvent(), "TopBackgroundEvent returns the originating event")
	assert.Same(t, bg, mgr.TopDialog(), "TopDialog returns the background instance")

	// Closing the top reveals the modal dialog underneath.
	mgr.handleClose()
	assert.True(t, mgr.Open(), "manager still has the modal dialog underneath")
	assert.False(t, mgr.TopIsBackground(), "underneath is the modal dialog, not background")
	assert.Nil(t, mgr.TopBackgroundEvent())
	assert.Same(t, modal, mgr.TopDialog())

	// Closing again empties the stack.
	mgr.handleClose()
	assert.False(t, mgr.Open())
	assert.False(t, mgr.TopIsBackground())
	assert.Nil(t, mgr.TopBackgroundEvent())
	assert.Nil(t, mgr.TopDialog())
}

// TestManagerHasDialog verifies HasDialog sees the whole stack, including
// dialogs buried under the topmost one, unlike TopDialog.
func TestManagerHasDialog(t *testing.T) {
	t.Parallel()

	mgr := New().(*manager)
	isExit := func(d Dialog) bool {
		_, ok := d.(*exitConfirmationDialog)
		return ok
	}

	assert.False(t, mgr.HasDialog(isExit), "empty manager matches nothing")

	mgr.handleOpen(OpenDialogMsg{Model: NewExitConfirmationDialog()})
	assert.True(t, mgr.HasDialog(isExit))

	// Bury it under another dialog: TopDialog no longer sees it, HasDialog does.
	mgr.handleOpen(OpenDialogMsg{Model: NewHelpDialog(nil)})
	_, topIsExit := mgr.TopDialog().(*exitConfirmationDialog)
	require.False(t, topIsExit)
	assert.True(t, mgr.HasDialog(isExit), "a buried dialog must still be found")

	assert.False(t, mgr.HasDialog(func(Dialog) bool { return false }))
}

// TestManagerClosePlanDetail verifies the targeted plan-detail close is
// idempotent: it pops the topmost dialog only when it is the detail for
// exactly the given ref, so duplicates, wrong refs, another dialog on top,
// or an already-closed detail never pop the wrong dialog.
func TestManagerClosePlanDetail(t *testing.T) {
	t.Parallel()

	ref := plans.SharedRef("release")
	newDetail := func() Dialog {
		return NewPlanDetailDialog(plans.Plan{Scope: plans.ScopeShared, Name: "release"})
	}

	t.Run("closes the matching top detail once, duplicates are no-ops", func(t *testing.T) {
		t.Parallel()
		mgr := New().(*manager)
		browser := NewPlanBrowserDialog(plans.ListResult{})
		mgr.handleOpen(OpenDialogMsg{Model: browser})
		mgr.handleOpen(OpenDialogMsg{Model: newDetail()})

		mgr.Update(ClosePlanDetailMsg{Ref: ref})
		require.Same(t, browser, mgr.TopDialog(), "the detail closes, the browser surfaces")

		// The duplicated close arrives after the detail already closed.
		mgr.Update(ClosePlanDetailMsg{Ref: ref})
		assert.Same(t, browser, mgr.TopDialog(), "a duplicate close must not pop the browser")

		mgr.handleClose()
		mgr.Update(ClosePlanDetailMsg{Ref: ref})
		assert.False(t, mgr.Open(), "a close on an empty stack is a no-op")
	})

	t.Run("a detail showing another plan is left alone", func(t *testing.T) {
		t.Parallel()
		mgr := New().(*manager)
		other := NewPlanDetailDialog(plans.Plan{Scope: plans.ScopeShared, Name: "other"})
		mgr.handleOpen(OpenDialogMsg{Model: other})

		mgr.Update(ClosePlanDetailMsg{Ref: ref})
		assert.Same(t, other, mgr.TopDialog(), "a close for a different ref is a no-op")
	})

	t.Run("a non-detail dialog on top is left alone", func(t *testing.T) {
		t.Parallel()
		mgr := New().(*manager)
		mgr.handleOpen(OpenDialogMsg{Model: newDetail()})
		help := NewHelpDialog(nil)
		mgr.handleOpen(OpenDialogMsg{Model: help})

		mgr.Update(ClosePlanDetailMsg{Ref: ref})
		assert.Same(t, help, mgr.TopDialog(), "the covering dialog must not be popped")
		assert.True(t, mgr.HasDialog(func(d Dialog) bool {
			viewer, ok := d.(PlanDetailViewer)
			return ok && viewer.PlanRef() == ref
		}), "the buried detail stays open")
	})
}

func TestTakeBackgroundDialogsPreservesOrderAndOtherLayers(t *testing.T) {
	t.Parallel()
	mgr := New().(*manager)
	first, second := new(int), new(int)
	modal := NewExitConfirmationDialog()
	one := NewElicitationDialog("one", nil, nil)
	two := NewElicitationDialog("two", nil, nil)
	mgr.handleOpen(OpenDialogMsg{Model: one, OriginatingEvent: first})
	mgr.handleOpen(OpenDialogMsg{Model: modal})
	mgr.handleOpen(OpenDialogMsg{Model: two, OriginatingEvent: second})
	mgr.drag.active = true

	removed := mgr.TakeBackgroundDialogs(func(tea.Msg) bool { return true })
	require.Len(t, removed, 2)
	assert.Same(t, one, removed[0].Model)
	assert.Same(t, first, removed[0].OriginatingEvent)
	assert.Same(t, two, removed[1].Model)
	assert.Same(t, second, removed[1].OriginatingEvent)
	assert.Same(t, modal, mgr.TopDialog())
	assert.False(t, mgr.drag.active)
	assert.Empty(t, mgr.TakeBackgroundDialogs(func(tea.Msg) bool { return true }))
}

func TestTakeBackgroundDialogsRetainsUnmatchedAttention(t *testing.T) {
	t.Parallel()
	mgr := New().(*manager)
	first, second := new(int), new(int)
	mgr.handleOpen(OpenDialogMsg{Model: NewElicitationDialog("one", nil, nil), OriginatingEvent: first})
	mgr.handleOpen(OpenDialogMsg{Model: NewElicitationDialog("two", nil, nil), OriginatingEvent: second})
	removed := mgr.TakeBackgroundDialogs(func(event tea.Msg) bool { return event == first })
	require.Len(t, removed, 1)
	assert.Same(t, first, removed[0].OriginatingEvent)
	assert.Same(t, second, mgr.TopBackgroundEvent())
}

func TestManagerSettingsTabsRemainClickableAfterDragging(t *testing.T) {
	t.Parallel()

	mgr := New().(*manager)
	mgr.Update(tea.WindowSizeMsg{Width: 140, Height: 60})
	d := NewSettingsDialog(messages.Preferences{}, true).(*settingsDialog)
	mgr.Update(OpenDialogMsg{Model: d})

	mgr.Update(settingsTabClick(t, d, "Behavior"))
	assert.Equal(t, tabBehavior, d.tab)
	assert.False(t, mgr.drag.active, "clicking a tab must not drag the dialog")

	row, col := d.Position()
	mgr.Update(tea.MouseClickMsg{X: col + 4, Y: row + 1, Button: tea.MouseLeft})
	require.True(t, mgr.drag.active, "the title must still be draggable")
	mgr.Update(tea.MouseMotionMsg{X: col + 7, Y: row + 3})
	mgr.Update(tea.MouseReleaseMsg{Button: tea.MouseLeft})

	click := settingsTabClick(t, d, "Notifications")
	click.X += 3
	click.Y += 2
	mgr.Update(click)
	assert.Equal(t, tabNotifications, d.tab)
	assert.False(t, mgr.drag.active)
	assert.Equal(t, 3, mgr.stack[0].offsetX)
	assert.Equal(t, 2, mgr.stack[0].offsetY)
}

type dialogScrollbarClock struct{ now time.Time }

func (c *dialogScrollbarClock) Now() time.Time { return c.now }
func (c *dialogScrollbarClock) Tick(delay time.Duration, deliver func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg {
		c.now = c.now.Add(delay)
		return deliver(c.now)
	}
}

func TestManagerRevealsScrollbarAfterAdjustedPointerAndScroll(t *testing.T) {
	d := newReadOnlyScrollDialog(readOnlyScrollDialogSize{
		widthPercent: 80, minWidth: 20, maxWidth: 60, heightPercent: 80, heightMax: 20,
	}, func(_, _ int) []string {
		return make([]string, 60)
	})
	clock := animation.NewRuntimeWithScheduler(&dialogScrollbarClock{now: time.Unix(1, 0)})
	mgr := NewWithAnimation(clock).(*manager)
	mgr.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	mgr.Update(OpenDialogMsg{Model: &d})
	mgr.stack[0].offsetX, mgr.stack[0].offsetY = 3, 2
	mgr.GetLayers()
	row, _ := d.Position()
	x := d.scrollview.ScrollbarX() + 3
	y := row + 7
	step := func(cmd tea.Cmd) tea.Cmd {
		t.Helper()
		require.NotNil(t, cmd)
		tick, ok := clock.Accept(cmd().(animation.TickMsg))
		require.True(t, ok)
		mgr.Update(tick)
		return clock.Continue()
	}

	hidden := mgr.View()
	_, cmd := mgr.Update(tea.MouseMotionMsg{X: x, Y: y})
	require.Equal(t, hidden, mgr.View())
	cmd = step(cmd)
	partial := mgr.View()
	require.NotEqual(t, hidden, partial, "manager translates pointer into the moved dialog")
	for cmd != nil {
		cmd = step(cmd)
	}
	full := mgr.View()
	require.NotEqual(t, partial, full, "dialog paints an intermediate fade-in frame")
	_, cmd = mgr.Update(tea.MouseMotionMsg{X: x - 10, Y: y})
	cmd = step(cmd)
	require.NotEqual(t, hidden, mgr.View())
	require.NotEqual(t, full, mgr.View(), "dialog paints an intermediate fade-out frame")

	mgr.Update(messages.WheelCoalescedMsg{X: x - 10, Y: y, Delta: 1})
	require.Positive(t, d.scrollview.ScrollOffset())
	for cmd != nil {
		cmd = step(cmd)
	}
	require.False(t, clock.HasActive())
}

func TestManagerDrawsSettingsScrollbarOnFirstResizedFrame(t *testing.T) {
	clock := animation.NewRuntimeWithScheduler(&dialogScrollbarClock{now: time.Unix(1, 0)})
	mgr := NewWithAnimation(clock).(*manager)
	mgr.Update(tea.WindowSizeMsg{Width: 140, Height: 24})
	d := NewSettingsDialog(messages.Preferences{}, true).(*settingsDialog)
	mgr.Update(OpenDialogMsg{Model: d})
	mgr.GetLayers()
	require.True(t, d.body.NeedsScrollbar())

	const y = 10
	cell := func(x int) string {
		layer := mgr.GetLayers()[0]
		line := strings.Split(layer.GetContent(), "\n")[y-layer.GetY()]
		col := x - layer.GetX()
		return ansi.Strip(ansi.Cut(line, col, col+1))
	}
	drain := func(cmd tea.Cmd) {
		t.Helper()
		for range 100 {
			if cmd == nil {
				break
			}
			tick, ok := clock.Accept(cmd().(animation.TickMsg))
			require.True(t, ok)
			mgr.Update(tick)
			cmd = clock.Continue()
		}
		require.False(t, clock.HasActive())
	}

	oldX := d.body.ScrollbarX()
	_, cmd := mgr.Update(tea.MouseMotionMsg{X: oldX, Y: y})
	drain(cmd)
	require.Equal(t, "│", cell(oldX))

	// The dialog is already at its maximum width, so 20 new columns move
	// the scrollbar exactly 10 columns without changing the body size.
	x := oldX + 10
	_, cmd = mgr.Update(tea.MouseMotionMsg{X: x, Y: y})
	drain(cmd)
	require.Equal(t, " ", cell(oldX), "the old scrollbar has faded out")

	mgr.Update(tea.WindowSizeMsg{Width: 160, Height: 24})
	first := cell(x)
	require.Equal(t, x, d.body.ScrollbarX(), "the resized track must be under the pointer")
	require.Equal(t, "│", first, "the first resized layer must paint the scrollbar")
}

func TestSettingsValueKeysRevealSnapBackToSelection(t *testing.T) {
	for _, key := range []tea.KeyPressMsg{
		{Code: tea.KeySpace, Text: " "},
		{Code: tea.KeyLeft},
		{Code: tea.KeyRight},
	} {
		t.Run(key.String(), func(t *testing.T) {
			d := NewSettingsDialog(messages.Preferences{}, true).(*settingsDialog)
			d.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
			clock := animation.NewRuntimeWithScheduler(&dialogScrollbarClock{now: time.Unix(1, 0)})
			d.body.SetAnimationRuntime(clock)
			d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
			require.Equal(t, rowFadeScrollEdges, d.selected[tabAppearance])
			d.View()
			require.True(t, d.body.NeedsScrollbar(), "the fixture must overflow")
			require.Zero(t, d.body.ScrollOffset())

			step := func(cmd tea.Cmd) tea.Cmd {
				t.Helper()
				require.NotNil(t, cmd)
				tick, ok := clock.Accept(cmd().(animation.TickMsg))
				require.True(t, ok)
				d.body.UpdateScrollbarAnimation(tick)
				return clock.Continue()
			}
			var cmd tea.Cmd
			for range 20 {
				_, wheel := d.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
				cmd = tea.Batch(cmd, wheel)
			}
			d.View()
			scrolled := d.body.ScrollOffset()
			require.Positive(t, scrolled, "manual scrolling moves the selection out of view")
			for cmd != nil {
				cmd = step(cmd)
			}
			require.False(t, clock.HasActive(), "the scrollbar has faded out")

			_, cmd = d.Update(key)
			require.Less(t, d.body.ScrollOffset(), scrolled, "the value key returns to the selection")
			require.True(t, clock.HasActive(), "the snap reveals the scrollbar")
			require.NotNil(t, cmd)
		})
	}
}

func TestManagerRoutesTicksAndTeardownToToolConfirmation(t *testing.T) {
	clock := animation.NewRuntimeWithScheduler(&dialogScrollbarClock{now: time.Unix(1, 0)})
	mgr := NewWithAnimation(clock).(*manager)
	mgr.Update(tea.WindowSizeMsg{Width: 80, Height: 40})
	args := make([]string, 40)
	for i := range args {
		args[i] = fmt.Sprintf("%q: %d", fmt.Sprintf("key_%02d", i), i)
	}
	open := func() {
		t.Helper()
		mgr.Update(OpenDialogMsg{Model: NewToolConfirmationDialog(clock, &runtime.ToolCallConfirmationEvent{
			ToolCall:       tools.ToolCall{ID: "x", Function: tools.FunctionCall{Name: "write", Arguments: "{" + strings.Join(args, ",") + "}"}},
			ToolDefinition: tools.Tool{Name: "write"},
		}, &service.EmbeddedSessionState{})})
		mgr.View()
		mgr.Update(tea.KeyPressMsg{Code: tea.KeyUp})
		require.True(t, clock.HasActive(), "keyboard scrolling reveals the arguments scrollbar")
	}

	open()
	cmd := clock.EnsureRunning()
	for range 200 {
		if cmd == nil {
			break
		}
		tick, ok := clock.Accept(cmd().(animation.TickMsg))
		require.True(t, ok)
		mgr.Update(tick)
		cmd = clock.Continue()
	}
	require.False(t, clock.HasActive(), "manager ticks reach the dialog and expire")

	mgr.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	require.True(t, clock.HasActive())
	mgr.Update(OpenDialogMsg{Model: NewExitConfirmationDialog()})
	require.False(t, clock.HasActive(), "a covered dialog releases its ticks")
	mgr.Update(CloseDialogMsg{})

	mgr.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	require.True(t, clock.HasActive())
	mgr.Update(CloseDialogMsg{})
	require.False(t, clock.HasActive(), "closing stops the dialog's animation")

	open()
	mgr.Update(CloseAllDialogsMsg{})
	require.False(t, clock.HasActive(), "closing all stops the dialog's animation")
}

func TestManagerMovesScrollbarAnimationWithWorkingDirSection(t *testing.T) {
	favorites := make([]string, 40)
	for i := range favorites {
		favorites[i] = fmt.Sprintf("/pinned/%02d", i)
	}
	clock := animation.NewRuntimeWithScheduler(&dialogScrollbarClock{now: time.Unix(1, 0)})
	mgr := NewWithAnimation(clock).(*manager)
	mgr.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	d := NewWorkingDirPickerDialog(t.Context(), nil, favorites, nil, "/").(*workingDirPickerDialog)
	mgr.Update(OpenDialogMsg{Model: d})
	mgr.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	require.Equal(t, sectionPinned, d.section)
	mgr.View()
	require.True(t, d.pinnedScroll.NeedsScrollbar(), "the fixture must overflow")

	mgr.Update(messages.WheelCoalescedMsg{Delta: 1})
	require.Positive(t, d.pinnedScroll.ScrollOffset())
	require.True(t, clock.HasActive(), "the active section reveals its scrollbar")
	mgr.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	require.Equal(t, sectionBrowse, d.section)
	require.False(t, clock.HasActive(), "leaving a section releases its scrollbar ticks")

	mgr.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	mgr.Update(messages.WheelCoalescedMsg{Delta: 1})
	require.True(t, clock.HasActive())
	mgr.ClearScrollbarPointer()
	require.False(t, clock.HasActive(), "pane and focus changes clear the active section")
}
