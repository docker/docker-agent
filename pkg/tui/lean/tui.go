package lean

import (
	"strconv"
	"strings"
	"sync"
	"time"

	ansi "github.com/charmbracelet/x/ansi"
)

type InputListener func(data string) (consume bool)

type TUI struct {
	terminal *ProcessTerminal
	children []Component
	focused  Focusable
	inputs   []InputListener

	previousLines     []string
	previousWidth     int
	previousHeight    int
	cursorRow         int
	hardwareCursorRow int
	maxLinesRendered  int
	previousViewport  int

	renderMu  sync.Mutex
	requested bool
	stopped   bool
}

func NewTUI(terminal *ProcessTerminal) *TUI { return &TUI{terminal: terminal} }
func (t *TUI) AddChild(child Component)     { t.children = append(t.children, child) }

func (t *TUI) SetFocus(f Focusable) {
	if t.focused != nil {
		t.focused.SetFocused(false)
	}
	t.focused = f
	if t.focused != nil {
		t.focused.SetFocused(true)
	}
}

func (t *TUI) AddInputListener(listener InputListener) { t.inputs = append(t.inputs, listener) }

func (t *TUI) Start() error {
	if err := t.terminal.Start(t.handleInput, t.RequestRender); err != nil {
		return err
	}
	t.RequestRender()
	return nil
}

func (t *TUI) Stop() {
	if t.stopped {
		return
	}
	t.stopped = true
	t.terminal.DrainInput(time.Second, 50*time.Millisecond)
	t.terminal.Stop()
}

func (t *TUI) RequestRender() {
	t.renderMu.Lock()
	if t.requested || t.stopped {
		t.renderMu.Unlock()
		return
	}
	t.requested = true
	t.renderMu.Unlock()
	go func() {
		time.Sleep(8 * time.Millisecond)
		t.renderMu.Lock()
		t.requested = false
		t.renderMu.Unlock()
		t.doRender()
	}()
}

func (t *TUI) handleInput(data string) {
	for _, input := range t.inputs {
		if input(data) {
			return
		}
	}
	if t.focused != nil {
		t.focused.HandleInput(data)
		t.RequestRender()
	}
}

func (t *TUI) render(width int) []string {
	var lines []string
	for _, child := range t.children {
		lines = append(lines, child.Render(width)...)
	}
	return lines
}

func (t *TUI) doRender() {
	if t.stopped {
		return
	}

	width := t.terminal.Columns()
	height := t.terminal.Rows()
	viewportTop := maxInt(0, t.maxLinesRendered-height)
	prevViewportTop := t.previousViewport
	hardwareCursorRow := t.hardwareCursorRow
	computeLineDiff := func(targetRow int) int {
		currentScreenRow := hardwareCursorRow - prevViewportTop
		targetScreenRow := targetRow - viewportTop
		return targetScreenRow - currentScreenRow
	}

	newLines := t.render(width)
	cursorPos := extractCursorPosition(newLines)
	newLines = applyLineResets(newLines)

	fullRender := func(clearScreen bool) {
		var buffer strings.Builder
		buffer.WriteString("\x1b[?2026h")
		if clearScreen {
			buffer.WriteString("\x1b[2J\x1b[H\x1b[3J")
		}
		for i, line := range newLines {
			if i > 0 {
				buffer.WriteString("\r\n")
			}
			buffer.WriteString(line)
		}
		buffer.WriteString("\x1b[?2026l")
		t.terminal.Write(buffer.String())
		t.cursorRow = maxInt(0, len(newLines)-1)
		t.hardwareCursorRow = t.cursorRow
		if clearScreen {
			t.maxLinesRendered = len(newLines)
		} else {
			t.maxLinesRendered = maxInt(t.maxLinesRendered, len(newLines))
		}
		t.previousViewport = maxInt(0, t.maxLinesRendered-height)
		t.positionHardwareCursor(cursorPos, len(newLines), height)
		t.previousLines = append([]string(nil), newLines...)
		t.previousWidth = width
		t.previousHeight = height
	}

	if len(t.previousLines) == 0 {
		fullRender(false)
		return
	}
	if t.previousWidth != width || (t.previousHeight != 0 && t.previousHeight != height) {
		fullRender(true)
		return
	}
	if len(newLines) < t.maxLinesRendered {
		fullRender(true)
		return
	}

	firstChanged, lastChanged := -1, -1
	maxLines := maxInt(len(newLines), len(t.previousLines))
	for i := range maxLines {
		oldLine, newLine := "", ""
		if i < len(t.previousLines) {
			oldLine = t.previousLines[i]
		}
		if i < len(newLines) {
			newLine = newLines[i]
		}
		if oldLine != newLine {
			if firstChanged == -1 {
				firstChanged = i
			}
			lastChanged = i
		}
	}

	appendedLines := len(newLines) > len(t.previousLines)
	if appendedLines {
		if firstChanged == -1 {
			firstChanged = len(t.previousLines)
		}
		lastChanged = len(newLines) - 1
	}
	appendStart := appendedLines && firstChanged == len(t.previousLines) && firstChanged > 0

	if firstChanged == -1 {
		t.positionHardwareCursor(cursorPos, len(newLines), height)
		t.previousViewport = maxInt(0, t.maxLinesRendered-height)
		t.previousHeight = height
		return
	}

	previousContentViewportTop := maxInt(0, len(t.previousLines)-height)
	if firstChanged < previousContentViewportTop {
		fullRender(true)
		return
	}

	var buffer strings.Builder
	buffer.WriteString("\x1b[?2026h")
	prevViewportBottom := prevViewportTop + height - 1
	moveTargetRow := firstChanged
	if appendStart {
		moveTargetRow = firstChanged - 1
	}
	if moveTargetRow > prevViewportBottom {
		currentScreenRow := maxInt(0, minInt(height-1, hardwareCursorRow-prevViewportTop))
		moveToBottom := height - 1 - currentScreenRow
		if moveToBottom > 0 {
			buffer.WriteString(moveDown(moveToBottom))
		}
		scroll := moveTargetRow - prevViewportBottom
		buffer.WriteString(strings.Repeat("\r\n", scroll))
		prevViewportTop += scroll
		viewportTop += scroll
		hardwareCursorRow = moveTargetRow
	}

	lineDiff := computeLineDiff(moveTargetRow)
	switch {
	case lineDiff > 0:
		buffer.WriteString(moveDown(lineDiff))
	case lineDiff < 0:
		buffer.WriteString(moveUp(-lineDiff))
	}
	if appendStart {
		buffer.WriteString("\r\n")
	} else {
		buffer.WriteString("\r")
	}

	renderEnd := minInt(lastChanged, len(newLines)-1)
	for i := firstChanged; i <= renderEnd; i++ {
		if i > firstChanged {
			buffer.WriteString("\r\n")
		}
		buffer.WriteString("\x1b[2K")
		buffer.WriteString(newLines[i])
	}

	buffer.WriteString("\x1b[?2026l")
	t.terminal.Write(buffer.String())
	t.cursorRow = maxInt(0, len(newLines)-1)
	t.hardwareCursorRow = renderEnd
	t.maxLinesRendered = maxInt(t.maxLinesRendered, len(newLines))
	t.previousViewport = maxInt(0, t.maxLinesRendered-height)
	t.positionHardwareCursor(cursorPos, len(newLines), height)
	t.previousLines = append([]string(nil), newLines...)
	t.previousWidth = width
	t.previousHeight = height
}

func (t *TUI) positionHardwareCursor(pos *cursorPosition, totalLines, height int) {
	if pos == nil {
		t.terminal.HideCursor()
		return
	}
	viewportTop := maxInt(0, totalLines-height)
	targetRow := pos.Row
	currentScreenRow := t.hardwareCursorRow - viewportTop
	targetScreenRow := targetRow - viewportTop
	var buffer strings.Builder
	buffer.WriteString("\x1b[?2026h")
	delta := targetScreenRow - currentScreenRow
	switch {
	case delta > 0:
		buffer.WriteString(moveDown(delta))
	case delta < 0:
		buffer.WriteString(moveUp(-delta))
	}
	buffer.WriteString("\r")
	if pos.Col > 0 {
		buffer.WriteString(moveRight(pos.Col))
	}
	buffer.WriteString("\x1b[?25h\x1b[?2026l")
	t.terminal.Write(buffer.String())
	t.hardwareCursorRow = targetRow
}

type cursorPosition struct{ Row, Col int }

func extractCursorPosition(lines []string) *cursorPosition {
	for row, line := range lines {
		before, after, found := strings.Cut(line, cursorMarker)
		if !found {
			continue
		}
		lines[row] = before + after
		return &cursorPosition{Row: row, Col: ansi.StringWidth(before)}
	}
	return nil
}

func applyLineResets(lines []string) []string {
	out := make([]string, len(lines))
	for i, line := range lines {
		out[i] = line + "\x1b[0m"
	}
	return out
}

func moveUp(n int) string {
	if n <= 0 {
		return ""
	}
	return "\x1b[" + strconv.Itoa(n) + "A"
}

func moveDown(n int) string {
	if n <= 0 {
		return ""
	}
	return "\x1b[" + strconv.Itoa(n) + "B"
}

func moveRight(n int) string {
	if n <= 0 {
		return ""
	}
	return "\x1b[" + strconv.Itoa(n) + "C"
}
