package dialog

import (
	"testing"

	"github.com/stretchr/testify/assert"
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
	bg := NewElicitationDialog("Pick a value", nil, nil)
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
