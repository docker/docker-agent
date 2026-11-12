package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/messages"
)

func (m *appModel) planSidebarEnabled() bool {
	return m.layoutSettings.ShowPlans && !m.leanMode && !m.hideSidebar
}

func (m *appModel) planDataVisible() bool {
	return m.planSidebarEnabled() || m.planDialogOpen()
}

func (m *appModel) refreshPlanSidebarCmd() tea.Cmd {
	if !m.planSidebarEnabled() {
		return nil
	}
	return m.planRefreshCmd(false)
}

func (m *appModel) updatePlanSidebar(data messages.PlanSidebarDataMsg) tea.Cmd {
	m.planSidebarData = data
	var cmds []tea.Cmd
	activeUpdated := false
	for _, tab := range m.tabs {
		if tab.chatPage == nil {
			continue
		}
		updated, effects := tab.chatPage.UpdateEffects(data)
		tab.chatPage = updated
		visible := tab == m.activeTab
		if visible {
			activeUpdated = true
		}
		cmds = append(cmds, effects.Cmd(visible))
	}
	if !activeUpdated && m.activeTab != nil && m.activeTab.chatPage != nil {
		cmds = append(cmds, m.updateChatCmd(data))
	}
	return tea.Batch(cmds...)
}

func (m *appModel) cancelSidebarPlanEdit() {
	m.sidebarPlanEditGeneration++
	m.sidebarPlanEditInFlight = false
}

func (m *appModel) handleEditSidebarPlan(msg messages.EditSidebarPlanMsg) (tea.Model, tea.Cmd) {
	if msg.TabID != "" && (m.supervisor == nil || m.supervisor.ActiveID() != msg.TabID) {
		return m, nil
	}
	if !m.planSidebarEnabled() || m.dialogMgr.Open() || m.sidebarPlanEditInFlight {
		return m, nil
	}
	m.sidebarPlanEditGeneration++
	m.sidebarPlanEditInFlight = true
	cmd := m.preparePlanEdit(messages.EditPlanMsg{
		Ref: msg.Ref, ExpectedVersion: msg.ExpectedVersion,
	}, m.sidebarPlanEditGeneration)
	return m, cmd
}
