package ui

import (
	"bufio"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

const (
	seqHideCursor            = "\x1b[?25l"
	seqShowCursor            = "\x1b[?25h"
	seqSyncStart             = "\x1b[?2026h"
	seqSyncEnd               = "\x1b[?2026l"
	seqEraseLine             = "\x1b[2K"
	seqClearScreen           = "\x1b[2J\x1b[H\x1b[3J"
	seqEnableBracketedPaste  = "\x1b[?2004h"
	seqDisableBracketedPaste = "\x1b[?2004l"
)

// Renderer draws the whole conversation as a single, growing array of lines and
// keeps it in sync with the terminal using minimal, differential updates. It
// writes to the normal screen buffer (never the alternate screen): content that
// scrolls off the top becomes immutable terminal scrollback, exactly like a
// regular program's output, while the bottom of the array (the input box and
// status footer) is rewritten in place.
//
// The model mirrors how Claude Code / pi render: each frame the controller
// produces the full set of lines; the renderer diffs them against the previous
// frame and rewrites only the changed rows within the visible viewport, letting
// the terminal scroll naturally when content is appended past the bottom.
type Renderer struct {
	w *bufio.Writer

	width  int
	height int

	prev        []string
	viewportTop int // index in prev of the topmost visible row
	cursorRow   int // buffer row the hardware cursor currently sits on
	initialized bool
	needsRedraw bool
}

func NewRenderer(w *bufio.Writer, width, height int) *Renderer {
	return &Renderer{w: w, width: width, height: height}
}

// SetSize records a new terminal size and forces a clean repaint on the next
// frame, since wrapping and the viewport both change with the dimensions.
func (r *Renderer) SetSize(width, height int) {
	r.width = width
	r.height = height
	r.needsRedraw = true
}

// Repaint forces the next frame to clear the screen and redraw from scratch.
func (r *Renderer) Repaint() {
	r.needsRedraw = true
}

// Frame reconciles the screen with newLines and places the hardware cursor at
// (cursorLine, cursorCol), where cursorLine is an index into newLines.
func (r *Renderer) Frame(newLines []string, cursorLine, cursorCol int) {
	if len(newLines) == 0 {
		newLines = []string{""}
	}

	if !r.initialized || r.needsRedraw {
		r.fullRedraw(newLines, cursorLine, cursorCol, r.initialized)
		r.initialized = true
		r.needsRedraw = false
		return
	}

	first, last := diffLines(r.prev, newLines)
	if first == -1 {
		// Content is unchanged; only the cursor may need to move.
		var b strings.Builder
		b.WriteString(seqSyncStart)
		b.WriteString(seqHideCursor)
		r.cursorRow = r.moveCursor(&b, r.cursorRow, cursorLine, cursorCol)
		b.WriteString(seqShowCursor)
		b.WriteString(seqSyncEnd)
		r.write(b.String())
		r.prev = newLines
		return
	}

	newViewportTop := max(0, len(newLines)-r.height)
	// Changes to a live block may start above the viewport. Repaint only the
	// visible rows rather than clearing terminal scrollback on every update.
	if first < r.viewportTop || newViewportTop < r.viewportTop {
		r.repaintVisible(newLines, cursorLine, cursorCol)
		return
	}

	renderEnd := min(last, len(newLines)-1)

	var b strings.Builder
	b.WriteString(seqSyncStart)
	b.WriteString(seqHideCursor)

	cur := r.cursorRow
	viewportBottom := r.viewportTop + r.height - 1

	// Move to the first changed row. If it sits one row below the visible area
	// (a fresh append onto a full screen), scroll a single line into view.
	if first-r.viewportTop <= r.height-1 {
		r.moveRow(&b, cur, first)
		b.WriteByte('\r')
	} else {
		r.moveRow(&b, cur, viewportBottom)
		b.WriteString("\r\n")
	}

	for i := first; i <= renderEnd; i++ {
		if i > first {
			b.WriteString("\r\n")
		}
		b.WriteString(seqEraseLine)
		b.WriteString(newLines[i])
	}
	cur = renderEnd

	// Clear stale trailing rows that remain when content shrank.
	for i := len(newLines); i < len(r.prev); i++ {
		b.WriteString("\r\n")
		b.WriteString(seqEraseLine)
		cur++
	}

	r.viewportTop = newViewportTop
	cur = r.moveCursor(&b, cur, cursorLine, cursorCol)

	b.WriteString(seqShowCursor)
	b.WriteString(seqSyncEnd)
	r.write(b.String())

	r.cursorRow = cur
	r.prev = newLines
}

// repaintVisible reconciles changes that start above the viewport without
// erasing the terminal's scrollback. Growth scrolls the screen before painting
// the new visible tail; shrinkage simply replaces the visible rows.
func (r *Renderer) repaintVisible(newLines []string, cursorLine, cursorCol int) {
	top := max(0, len(newLines)-r.height)
	var b strings.Builder
	b.WriteString(seqSyncStart)
	b.WriteString(seqHideCursor)
	if delta := top - r.viewportTop; delta > 0 {
		r.moveRow(&b, r.cursorRow, r.viewportTop+r.height-1)
		for range delta {
			b.WriteString("\r\n")
		}
	}
	b.WriteString("\x1b[H")
	for i := range r.height {
		if i > 0 {
			b.WriteString("\r\n")
		}
		b.WriteString(seqEraseLine)
		if top+i < len(newLines) {
			b.WriteString(newLines[top+i])
		}
	}
	r.viewportTop = top
	cur := r.moveCursor(&b, top+r.height-1, cursorLine, cursorCol)
	b.WriteString(seqShowCursor)
	b.WriteString(seqSyncEnd)
	r.write(b.String())
	r.cursorRow = cur
	r.prev = newLines
}

// fullRedraw repaints every line. When wipe is set it also clears the screen
// and scrollback first (used on resize); otherwise it assumes a clean line and
// streams the content out, letting the terminal scroll as needed.
func (r *Renderer) fullRedraw(newLines []string, cursorLine, cursorCol int, wipe bool) {
	var b strings.Builder
	b.WriteString(seqSyncStart)
	b.WriteString(seqHideCursor)
	if wipe {
		b.WriteString(seqClearScreen)
	} else {
		b.WriteByte('\r')
	}

	for i, line := range newLines {
		if i > 0 {
			b.WriteString("\r\n")
		}
		b.WriteString(seqEraseLine)
		b.WriteString(line)
	}

	r.viewportTop = max(0, len(newLines)-r.height)
	cur := r.moveCursor(&b, len(newLines)-1, cursorLine, cursorCol)

	b.WriteString(seqShowCursor)
	b.WriteString(seqSyncEnd)
	r.write(b.String())

	r.cursorRow = cur
	r.prev = newLines
}

// EraseBelow drops everything from buffer row `line` downward (the interactive
// chrome), leaving the cursor on that now-blank row so the shell prompt returns
// directly beneath the conversation. Used on exit.
func (r *Renderer) EraseBelow(line int) {
	lo := r.viewportTop
	hi := r.viewportTop + r.height - 1
	if line < lo {
		line = lo
	}
	if line > hi {
		line = hi
	}

	var b strings.Builder
	b.WriteString(seqSyncStart)
	r.moveRow(&b, r.cursorRow, line)
	b.WriteByte('\r')
	b.WriteString(ansi.EraseDisplay(0))
	b.WriteString(seqShowCursor)
	b.WriteString(seqSyncEnd)
	r.write(b.String())

	r.cursorRow = line
}

// moveRow emits a vertical cursor move from buffer row "from" to "to". Both
// rows are assumed to be within the visible viewport.
func (r *Renderer) moveRow(b *strings.Builder, from, to int) {
	switch d := to - from; {
	case d > 0:
		b.WriteString(ansi.CursorDown(d))
	case d < 0:
		b.WriteString(ansi.CursorUp(-d))
	}
}

// moveCursor positions the hardware cursor at (line, col), clamping both the
// current and target rows to the visible viewport, and returns the row it ended on.
func (r *Renderer) moveCursor(b *strings.Builder, from, line, col int) int {
	lo := r.viewportTop
	hi := r.viewportTop + r.height - 1
	if from < lo {
		from = lo
	}
	if from > hi {
		from = hi
	}
	if line < lo {
		line = lo
	}
	if line > hi {
		line = hi
	}
	r.moveRow(b, from, line)
	b.WriteByte('\r')
	if col > 0 {
		b.WriteString(ansi.CursorForward(col))
	}
	return line
}

func (r *Renderer) ViewportTop() int {
	return r.viewportTop
}

func (r *Renderer) write(s string) {
	_, _ = r.w.WriteString(s)
	_ = r.w.Flush()
}

// diffLines returns the first and last indices at which prev and next differ,
// or (-1, -1) when they are identical.
func diffLines(prev, next []string) (first, last int) {
	first, last = -1, -1
	n := max(len(prev), len(next))
	for i := range n {
		var p, x string
		if i < len(prev) {
			p = prev[i]
		}
		if i < len(next) {
			x = next[i]
		}
		if p != x {
			if first == -1 {
				first = i
			}
			last = i
		}
	}
	return first, last
}
