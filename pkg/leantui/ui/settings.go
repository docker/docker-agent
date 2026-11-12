package ui

// SettingsModel is the inline settings panel shown in place of the editor.
type SettingsModel struct {
	Rows        []SettingRow
	Selected    int
	ConfirmYOLO bool
}

type SettingRow struct {
	Label string
	Value string
}

func (s *SettingsModel) Render(width int) []string {
	lines := []string{Truncate(StBold().Render("Settings"), width)}
	for i, row := range s.Rows {
		prefix, style := "  ", StMuted()
		if i == s.Selected {
			prefix, style = "› ", StPrimary()
		}
		lines = append(lines, Truncate(style.Render(prefix+row.Label+": "+row.Value), width))
	}

	return append(lines, Truncate(StMuted().Render("↑/↓ navigate · ←/→/space change · Enter save · Esc cancel"), width))
}
