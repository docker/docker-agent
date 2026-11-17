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
	return m.updateAllChatsCmd(messages.ImageRenderingChangedMsg{})
}
