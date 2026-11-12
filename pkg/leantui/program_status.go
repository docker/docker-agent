package leantui

func (m *model) publishProgramStatus() {
	if m.programStatus == nil || m.term == nil || m.programStatusProbe || m.programStatusUnsupported {
		return
	}
	m.programStatus.Set(m.statusSession.Status())
	_, _ = m.term.Writer().WriteString(m.programStatus.String())
	_ = m.term.Writer().Flush()
}
