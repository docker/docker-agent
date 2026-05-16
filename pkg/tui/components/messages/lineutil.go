package messages

import (
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
)

// styleLineSegment applies a lipgloss style to the portion of a line between
// startCol and endCol (display columns), preserving the text before and after.
// ANSI codes in the styled segment are stripped so the style renders cleanly.
func styleLineSegment(line string, startCol, endCol int, style lipgloss.Style) string {
	plainLine := ansi.Strip(line)
	plainWidth := runewidth.StringWidth(plainLine)

	if startCol >= plainWidth || startCol >= endCol {
		return line
	}
	endCol = min(endCol, plainWidth)

	before := ansi.Cut(line, 0, startCol)
	segment := ansi.Strip(ansi.Cut(line, startCol, endCol))
	after := ansi.Cut(line, endCol, plainWidth)

	return before + style.Render(segment) + after
}
