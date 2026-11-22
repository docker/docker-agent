package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
)

type chatScrollbarPointer interface {
	SetScrollbarPointer(x, y int) tea.Cmd
	ClearScrollbarPointer()
	IsScrollbarDragging() bool
}

func chatScrollbarDragging(page chat.Page) bool {
	owner, ok := page.(chatScrollbarPointer)
	return ok && owner.IsScrollbarDragging()
}

func (m *appModel) updateScrollbarPointer(msg tea.Msg) tea.Cmd {
	page := m.activeTab.chatPage
	owner, ok := page.(chatScrollbarPointer)
	if _, blurred := msg.(tea.BlurMsg); blurred {
		m.clearDialogScrollbarPointer()
		if ok {
			owner.ClearScrollbarPointer()
		}
		return nil
	}
	if !ok || m.dialogMgr.Open() || m.paneHidden {
		return nil
	}
	var x, y int
	switch event := msg.(type) {
	case tea.MouseMsg:
		x, y = event.Mouse().X, event.Mouse().Y
	case messages.WheelCoalescedMsg:
		x, y = event.X, event.Y
	default:
		return nil
	}
	if m.hitTestRegion(y) != regionContent {
		x, y = -1, -1
	}
	return owner.SetScrollbarPointer(x, y)
}

func (m *appModel) clearDialogScrollbarPointer() {
	if dialog, ok := m.dialogMgr.(interface{ ClearScrollbarPointer() }); ok {
		dialog.ClearScrollbarPointer()
	}
}

func (m *appModel) clearInactiveScrollbarPointer(previous chat.Page) {
	if m.paneHidden {
		m.clearDialogScrollbarPointer()
	}
	if previous != m.activeTab.chatPage || m.dialogMgr.Open() || m.paneHidden {
		if owner, ok := previous.(chatScrollbarPointer); ok {
			owner.ClearScrollbarPointer()
		}
	}
}
