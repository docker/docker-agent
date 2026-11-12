package tui

import (
	tea "charm.land/bubbletea/v2"

	tuiimage "github.com/docker/docker-agent/pkg/tui/image"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func (m *appModel) syncImageRendering(before bool) tea.Cmd {
	enabled := m.imageWriter.RenderingEnabled()
	tuiimage.SetRenderingEnabled(enabled)
	if before == enabled {
		return nil
	}
	m.viewCacheValid = false
	var cmds []tea.Cmd
	activeUpdated := false
	for _, tab := range m.tabs {
		if tab.chatPage == nil {
			continue
		}
		updated, effects := tab.chatPage.UpdateEffects(messages.ImageRenderingChangedMsg{})
		tab.chatPage = updated
		visible := tab == m.activeTab
		activeUpdated = activeUpdated || visible
		cmds = append(cmds, effects.Cmd(visible))
	}
	if !activeUpdated && m.activeTab != nil && m.activeTab.chatPage != nil {
		cmds = append(cmds, m.updateChatCmd(messages.ImageRenderingChangedMsg{}))
	}
	return tea.Batch(cmds...)
}
