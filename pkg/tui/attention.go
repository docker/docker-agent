package tui

import (
	"slices"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func isTabStateEvent(msg tea.Msg) bool {
	switch msg.(type) {
	case runtime.Event, messages.StreamCancelledMsg:
		return true
	default:
		return false
	}
}

// applyTabEvent runs only in Update, where visibility and event order are authoritative.
func (m *appModel) applyTabEvent(tab *tabModel, msg tea.Msg) tea.Cmd {
	if tab == nil || tab.state == nil || !isTabStateEvent(msg) {
		return nil
	}
	active := tab == m.activeTab
	changed, bell := tab.state.Apply(msg, active)
	for event := range tab.attentionDialogs {
		if tab.state.RetiresAttention(msg, event) {
			delete(tab.attentionDialogs, event)
		}
	}
	if active {
		m.dialogMgr.TakeBackgroundDialogs(func(event tea.Msg) bool {
			return tab.state.RetiresAttention(msg, event)
		})
	}
	var cmds []tea.Cmd
	if changed {
		cmds = append(cmds, m.refreshTabs())
	}
	if bell {
		cmds = append(cmds, core.CmdHandler(messages.BellMsg{}))
	}
	return tea.Batch(cmds...)
}

func (m *appModel) refreshTabs() tea.Cmd {
	if m.supervisor == nil || m.tabBar == nil {
		return nil
	}
	tabs, active := m.supervisor.GetTabs()
	previous := m.tabBar.Height()
	m.tabBar.SetTabs(tabs, active)
	m.statusBar.SetShowNewTab(m.tabBar.Height() == 0)
	if previous != m.tabBar.Height() {
		return m.resizeAll()
	}
	return nil
}

// replayPendingEvent consumes prompts only as they are opened, synchronously and in FIFO order.
func (m *appModel) replayPendingEvent(tabID string) tea.Cmd {
	tab := m.tabs[tabID]
	if tab == nil || tab != m.activeTab || tab.chatPage == nil || tab.sessionState == nil || tab.state == nil {
		return nil
	}
	runner := m.supervisor.GetRunner(tabID)
	if runner == nil {
		return nil
	}
	var cmds []tea.Cmd
	for {
		event := tab.state.Consume()
		if event == nil {
			break
		}
		model := tab.attentionDialogs[event]
		delete(tab.attentionDialogs, event)
		if model == nil {
			model = dialog.NewAttentionDialog(m.ctx(), m.ar, runner.App, tab.sessionState, event)
		}
		if model != nil {
			cmds = append(cmds, m.updateDialogCmd(dialog.OpenDialogMsg{Model: model, OriginatingEvent: event}))
		}
	}
	tab.attentionDialogs = nil
	return tea.Batch(cmds...)
}

// parkAttention removes the whole attention stack before the incoming tab can open its prompts.
func (m *appModel) parkAttention(tab *tabModel) {
	opened := m.dialogMgr.TakeBackgroundDialogs(func(tea.Msg) bool { return true })
	if tab == nil || tab.state == nil {
		return
	}
	if tab.attentionDialogs == nil {
		tab.attentionDialogs = make(map[tea.Msg]dialog.Dialog)
	}
	for _, entry := range slices.Backward(opened) {
		tab.state.Prepend(entry.OriginatingEvent)
		tab.attentionDialogs[entry.OriginatingEvent] = entry.Model
	}
}

func (m *appModel) retireAttention(tab *tabModel) {
	if tab.state != nil {
		tab.state.ClearAttention()
	}
	tab.attentionDialogs = nil
	if tab == m.activeTab {
		m.dialogMgr.TakeBackgroundDialogs(func(tea.Msg) bool { return true })
	}
}

// Rotate before the app emits startup events for its replacement conversation.
func (m *appModel) preparePageReplacement(tabID string) {
	m.supervisor.RetirePage(tabID)
	if tab := m.tabs[tabID]; tab != nil {
		m.retireAttention(tab)
	}
}
