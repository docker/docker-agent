// Package ansiline edits ANSI-styled lines in terminal cells.
package ansiline

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// Splice overwrites line content at column x with overlay, preserving
// ANSI styling on both sides and padding when the line is shorter than x.
func Splice(line, overlay string, x int) string {
	left := ansi.Truncate(line, x, "")
	if pad := x - ansi.StringWidth(left); pad > 0 {
		left += strings.Repeat(" ", pad)
	}
	cut := x + ansi.StringWidth(overlay)
	right := ""
	if lineWidth := ansi.StringWidth(line); cut < lineWidth {
		right = ansi.TruncateLeft(line, cut, "")
		// A wide rune straddling the cut is kept whole by TruncateLeft,
		// which would shift the rest of the line one cell to the right.
		// Cut the straddled rune entirely and pad its uncovered cell with
		// a space — what a terminal shows for a half-overwritten cell.
		if extra := ansi.StringWidth(right) - (lineWidth - cut); extra > 0 {
			right = ansi.TruncateLeft(right, extra+1, "")
			right = strings.Repeat(" ", lineWidth-cut-ansi.StringWidth(right)) + right
		}
	}
	return left + overlay + right
}
