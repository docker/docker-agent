package tui

// The callback checks persistence independently; pending lazy restores cannot be
// reported until their actual session has been loaded.
func (m *appModel) updateProgramResume() {
	if m.resumeUpdate == nil || m.supervisor == nil || m.application == nil {
		return
	}
	select {
	case <-m.shutdownDone:
		return
	default:
	}
	tab := m.tabs[m.supervisor.ActiveID()]
	pending := tab != nil && tab.pendingRestore != nil
	m.resumeUpdate(m.application, pending)
}

func (m *appModel) clearProgramResume() {
	if m.resumeClear == nil {
		return
	}
	select {
	case <-m.shutdownDone:
		return
	default:
	}
	m.resumeClear()
	m.resumeUpdate = nil
	m.resumeClear = nil
	m.resumeContinued = nil
}
