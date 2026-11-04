package messages

import (
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// styleLineSegment applies a lipgloss style to the portion of a line between
// startCol and endCol (display columns), preserving the text before and after.
// ANSI codes in the styled segment are stripped so the style renders cleanly.
func styleLineSegment(line string, startCol, endCol int, style lipgloss.Style) string {
	plainWidth := ansi.StringWidth(line)

	if startCol >= plainWidth || startCol >= endCol {
		return line
	}
	endCol = min(endCol, plainWidth)

	before := ansi.Cut(line, 0, startCol)
	segment := ansi.Strip(ansi.Cut(line, startCol, endCol))
	after := ansi.Cut(line, endCol, plainWidth)

	return before + style.Render(segment) + after
}

// lineGrapheme keeps terminal columns aligned with whole grapheme clusters.
type lineGrapheme struct {
	text             string
	startCol, endCol int
}

func lineGraphemes(line string) []lineGrapheme {
	var clusters []lineGrapheme
	col := 0
	for line != "" {
		cluster, width := ansi.FirstGraphemeCluster(line, ansi.GraphemeWidth)
		clusters = append(clusters, lineGrapheme{text: cluster, startCol: col, endCol: col + width})
		line = line[len(cluster):]
		col += width
	}
	return clusters
}
