package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/components/completion"
	"github.com/docker/docker-agent/pkg/tui/components/editor"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
)

// The Bubble Tea Update contract returns a tea.Model interface, which forces
// us to type-assert the result back to the concrete sub-model type before
// reassigning it. The helpers below centralise that boilerplate.
//
// Two flavours are provided:
//
//   - updateXCmd: forwards a message to a sub-model and returns the produced
//     tea.Cmd. Use this when several sub-models are updated within the same
//     handler so their commands can be batched.
//
//   - forwardX: same forwarding, but returns (m, cmd) so that handlers that
//     only update a single sub-model can simply `return m.forwardX(msg)`.
//     This avoids the gocritic evalOrder warning that fires on
//     `return m, m.updateX(msg)` because m is mutated by the call.

// updateChatCmd forwards a message to the chat page and returns its cmd.
func (m *appModel) updateChatCmd(msg tea.Msg) tea.Cmd {
	updated, cmd := m.chatPage.Update(msg)
	m.chatPage = updated.(chat.Page)
	return cmd
}

// updateEditorCmd forwards a message to the editor and returns its cmd.
func (m *appModel) updateEditorCmd(msg tea.Msg) tea.Cmd {
	updated, cmd := m.editor.Update(msg)
	m.editor = updated.(editor.Editor)
	return cmd
}

// updateDialogCmd forwards a message to the dialog manager and returns its cmd.
func (m *appModel) updateDialogCmd(msg tea.Msg) tea.Cmd {
	updated, cmd := m.dialogMgr.Update(msg)
	m.dialogMgr = updated.(dialog.Manager)
	return cmd
}

// updateCompletionsCmd forwards a message to the completion manager and
// returns its cmd.
func (m *appModel) updateCompletionsCmd(msg tea.Msg) tea.Cmd {
	updated, cmd := m.completions.Update(msg)
	m.completions = updated.(completion.Manager)
	return cmd
}

// forwardChat is a convenience for handlers whose entire response is to
// forward the message to the chat page.
func (m *appModel) forwardChat(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := m.updateChatCmd(msg)
	return m, cmd
}

// forwardEditor is a convenience for handlers whose entire response is to
// forward the message to the editor.
func (m *appModel) forwardEditor(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := m.updateEditorCmd(msg)
	return m, cmd
}

// forwardDialog is a convenience for handlers whose entire response is to
// forward the message to the dialog manager.
func (m *appModel) forwardDialog(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := m.updateDialogCmd(msg)
	return m, cmd
}

// forwardCompletions is a convenience for handlers whose entire response is
// to forward the message to the completion manager.
func (m *appModel) forwardCompletions(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := m.updateCompletionsCmd(msg)
	return m, cmd
}
