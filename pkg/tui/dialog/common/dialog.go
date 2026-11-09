// Package common provides reusable dialog models, messages, and rendering helpers.
package common

import (
	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/core/layout"
)

// OpenDialogMsg is sent to open a new dialog.
//
// OriginatingEvent is an optional runtime event whose presence marks the
// dialog as a background dialog. Background dialogs do not block tab
// navigation: tab-switch keys and tab-bar mouse clicks keep working. When
// the user switches away from the tab that opened the dialog, the dialog is
// parked on its owning tab and re-displayed when the user returns. Other input (including
// mouse-wheel events) is still routed to the dialog while it is on screen.
type OpenDialogMsg struct {
	Model            Dialog
	OriginatingEvent tea.Msg
}

// CloseDialogMsg is sent to close the current (topmost) dialog
type CloseDialogMsg struct{ ElicitationID string }

// CloseAllDialogsMsg is sent to close all dialogs in the stack
type CloseAllDialogsMsg struct{}

// Broadcastable marks messages the manager delivers to every dialog in the
// stack instead of only the topmost one. Data-refresh messages implement it
// so dialogs buried under another dialog (e.g. the plan browser under its
// detail dialog) stay fresh.
type Broadcastable interface {
	BroadcastToDialogs()
}

// Dialog defines the interface that all dialogs must implement
type Dialog interface {
	layout.Model
	Position() (int, int) // Returns (row, col) for dialog placement
}

// CenterPosition calculates the centered position for a dialog given screen and dialog dimensions.
// Returns (row, col) suitable for use in Dialog.Position().
func CenterPosition(screenWidth, screenHeight, dialogWidth, dialogHeight int) (row, col int) {
	col = max(0, (screenWidth-dialogWidth)/2)
	row = max(0, (screenHeight-dialogHeight)/2)

	return row, col
}
