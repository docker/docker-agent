package ansiline

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/parser"
)

// PaintColumn overwrites cell x of rows [start, end) with a one-cell overlay.
// It writes the frame directly, without allocating the individual edited rows.
func PaintColumn(frame, overlay string, x, start, end int) string {
	if start >= end {
		return frame
	}
	var out strings.Builder
	// Account for replaying ANSI styles to the right of each painted cell.
	out.Grow(len(frame) + (end-start)*(len(overlay)+256))
	var controls [1024]byte
	prefix := controls[:0]
	for y := range end {
		line, rest, more := strings.Cut(frame, "\n")
		if y >= start {
			prefix = paintCell(&out, line, overlay, x, prefix[:0])
		} else {
			out.WriteString(line)
		}
		if !more {
			return out.String()
		}
		out.WriteByte('\n')
		frame = rest
	}
	out.WriteString(frame)
	return out.String()
}

func paintCell(out *strings.Builder, line, overlay string, x int, prefix []byte) []byte {
	state := parser.GroundState
	col, leftWidth := 0, 0
	leftDone := false
	rightStart, rightPad := len(line), 0
	for i := 0; i < len(line); {
		next, action := parser.Table.Transition(state, line[i])
		if action != parser.PrintAction && next != parser.Utf8State {
			if action != parser.ExecuteAction {
				// Keep trailing style/link sequences on the left, and replay
				// leading ones on the right after the overlay's style reset.
				if leftDone {
					out.WriteByte(line[i])
				}
				if rightStart == len(line) {
					prefix = append(prefix, line[i])
				}
			}
			state = next
			i++
			continue
		}

		// ASCII normally takes one cell, but can start a cluster such
		// as a base letter with combining marks or a keycap emoji.
		size, width := 1, 1
		if next == parser.Utf8State || i+1 < len(line) && line[i+1] >= 0xc2 {
			cluster, cells := ansi.FirstGraphemeCluster(line[i:], ansi.GraphemeWidth)
			size, width = len(cluster), cells
		}
		state = parser.GroundState
		if !leftDone {
			if col+width <= x {
				leftWidth = col + width
			} else {
				out.WriteString(line[:i])
				leftDone = true
			}
		}
		if rightStart == len(line) {
			if col > x && width > 0 {
				rightStart = i
			} else if col <= x && col+width > x+1 {
				rightPad = col + width - x - 1
			}
		}
		col += width
		i += size
	}
	if !leftDone {
		out.WriteString(line)
	}
	for range x - leftWidth {
		out.WriteByte(' ')
	}
	out.WriteString(overlay)
	for range rightPad {
		out.WriteByte(' ')
	}
	if col > x+1 {
		out.Write(prefix)
		out.WriteString(line[rightStart:])
	}
	return prefix
}
