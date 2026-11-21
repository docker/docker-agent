package leantui

// buildLines produces the entire frame and cursor position.
func (m *model) buildLines() (lines []string, cursorLine, cursorCol int) {
	m.screen.Status = m.status
	return m.screen.Frame(m.width, m.height, m.spinnerElapsed, m.busy, m.sessionState, m.pendingUsers)
}
