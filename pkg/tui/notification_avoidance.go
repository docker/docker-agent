package tui

import (
	"image"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/components/editor"
)

func (m *appModel) syncNotificationAvoidance() tea.Cmd {
	// Lean mode has no overlay compositor. Don't inspect the editor unless
	// an overlay can be displayed, and release scratch storage when it closes.
	if !m.notification.Open() || m.leanMode || !m.ready || m.err != nil || m.notificationStopped || m.activeTab == nil {
		m.notificationOccupied = nil
		return nil
	}
	select {
	case <-m.shutdownDone:
		return nil
	default:
	}
	cells := m.notificationOccupied[:0]
	if m.focusedPanel == PanelEditor && !m.dialogMgr.Open() && !m.paneHidden && m.editorTop() < m.height {
		if input, ok := m.activeTab.editor.(editor.TextOccupancy); ok {
			cells = input.AppendOccupiedTextCells(cells, image.Pt(0, m.editorTop()))
		}
	}
	m.notificationOccupied = cells
	return m.notification.SetAvoidance(m.ar, cells)
}
