package ansiline

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
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
	col, leftWidth := 0, 0
	leftDone := false
	rightPad := 0
	var right string
	var rightControls strings.Builder
	for i := 0; i < len(line); {
		// ASCII normally takes one cell, but can start a cluster
		// such as a base letter with combining marks or a keycap emoji.
		c := line[i]
		size, width := 1, 1
		switch {
		case c < ' ' || c >= 0x7f && c < 0xc0:
			seq, _, n, state := ansi.DecodeSequence(line[i:], ansi.NormalState, nil)
			if state == ansi.NormalState && replayable(seq) {
				if leftDone {
					out.WriteString(seq)
				}
				if right == "" {
					prefix = append(prefix, seq...)
				}
			} else if leftDone && right == "" {
				// Keep controls after the overwritten cluster at their original column.
				rightControls.WriteString(seq)
			}
			i += max(1, n)
			continue
		case c >= 0xc0 || i+1 < len(line) && line[i+1] >= 0xc2:
			cluster, cells := ansi.FirstGraphemeCluster(line[i:], ansi.GraphemeWidth)
			size, width = len(cluster), cells
		}
		if !leftDone {
			if col+width <= x {
				leftWidth = col + width
			} else {
				out.WriteString(line[:i])
				leftDone = true
			}
		}
		if right == "" {
			if col > x && width > 0 {
				right = line[i:]
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
	out.WriteString(rightControls.String())
	if right != "" {
		out.Write(prefix)
		out.WriteString(right)
	}
	return prefix
}

// Only SGR and OSC 8 establish styling that needs to cross the overlay.
func replayable(seq string) bool {
	switch {
	case ansi.HasCsiPrefix(seq) && strings.HasSuffix(seq, "m"):
		params := seq[1 : len(seq)-1]
		if seq[0] == ansi.ESC {
			params = params[1:]
		}
		for i := range len(params) {
			if c := params[i]; c < '0' || c > ';' {
				return false
			}
		}
		return true
	case ansi.HasOscPrefix(seq):
		if seq[0] == ansi.ESC {
			seq = seq[1:]
		}
		return strings.HasPrefix(seq[1:], "8;")
	default:
		return false
	}
}
