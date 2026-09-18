package message

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func styledAssistantLines(style lipgloss.Style, width int, content string) []string {
	if content == "" {
		return nil
	}
	// GetTabWidth cannot distinguish the default from an explicit TabWidth(0).
	if width < 4 || strings.ContainsRune(content, '\t') || !plainAssistantFrame(style) {
		return strings.Split(strings.TrimSuffix(style.Width(width).Render(content), "\n"), "\n")
	}

	var text ansi.Style
	if fg := style.GetForeground(); fg != (lipgloss.NoColor{}) {
		text = text.ForegroundColor(fg)
	}
	content = strings.ReplaceAll(content, "\r\n", "\n")
	// Wrap also closes and reopens ANSI styles and links across line breaks.
	content = lipgloss.Wrap(content, width-3, "")
	lines := strings.Split(content, "\n")
	widths := make([]int, len(lines))
	widest := width - 1
	for i, line := range lines {
		// Measure after styling/padding: incomplete escapes can consume either.
		lines[i] = " " + text.Styled(line) + " "
		widths[i] = ansi.StringWidth(lines[i])
		widest = max(widest, widths[i])
	}
	padding := strings.Repeat(" ", widest)
	for i, line := range lines {
		var border ansi.Style
		if fg := style.GetBorderLeftForeground(); fg != (lipgloss.NoColor{}) {
			border = border.ForegroundColor(fg)
		}
		lines[i] = border.Styled(" ") + line + padding[:widest-widths[i]]
	}
	return lines
}

// Mirror Style.Render's inputs; unused edges, vertical alignment and whitespace
// colors cannot affect this foreground-only, left-bordered frame.
func plainAssistantFrame(s lipgloss.Style) bool {
	link, _ := s.GetHyperlink()
	return s.Value() == "" && s.GetTransform() == nil && link == "" &&
		!s.GetBold() && !s.GetItalic() && !s.GetUnderline() && !s.GetStrikethrough() &&
		!s.GetReverse() && !s.GetBlink() && !s.GetFaint() && !s.GetInline() &&
		!s.GetUnderlineSpaces() && !s.GetStrikethroughSpaces() &&
		s.GetBackground() == (lipgloss.NoColor{}) && s.GetUnderlineColor() == (lipgloss.NoColor{}) &&
		s.GetHeight() == 0 && s.GetMaxHeight() == 0 && s.GetMaxWidth() == 0 &&
		s.GetAlignHorizontal() == lipgloss.Left &&
		s.GetPaddingTop() == 0 && s.GetPaddingBottom() == 0 &&
		s.GetPaddingLeft() == 1 && s.GetPaddingRight() == 1 && s.GetPaddingChar() == ' ' &&
		s.GetMarginTop() == 0 && s.GetMarginBottom() == 0 && s.GetMarginLeft() == 0 && s.GetMarginRight() == 0 &&
		s.GetBorderStyle() == lipgloss.HiddenBorder() && s.GetBorderLeft() &&
		!s.GetBorderTop() && !s.GetBorderBottom() && !s.GetBorderRight() &&
		s.GetBorderLeftBackground() == (lipgloss.NoColor{}) && len(s.GetBorderForegroundBlend()) == 0
}
