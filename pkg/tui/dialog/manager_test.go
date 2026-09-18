package dialog

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/plans"
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
