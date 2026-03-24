package lean

import (
	"strings"
	"unicode"
	"unicode/utf8"

	ansi "github.com/charmbracelet/x/ansi"
)

const cursorMarker = "\x1b_pi:c\a"

type Focusable interface {
	HandleInput(data string)
	SetFocused(focused bool)
	Focused() bool
}

type Editor struct {
	lines         []string
	cursorLine    int
	cursorCol     int
	focused       bool
	paddingX      int
	disableSubmit bool
	borderColor   func(...string) string
	onSubmit      func(string)
}

func NewEditor() *Editor {
	return &Editor{lines: []string{""}, paddingX: 1, borderColor: blueStyle.Render}
}

func (e *Editor) Invalidate()                              {}
func (e *Editor) Focused() bool                            { return e.focused }
func (e *Editor) SetFocused(focused bool)                  { e.focused = focused }
func (e *Editor) SetBorderColor(fn func(...string) string) { e.borderColor = fn }
func (e *Editor) SetDisableSubmit(disable bool)            { e.disableSubmit = disable }
func (e *Editor) OnSubmit(fn func(string))                 { e.onSubmit = fn }
func (e *Editor) Value() string                            { return strings.Join(e.lines, "\n") }

func (e *Editor) Reset() {
	e.lines = []string{""}
	e.cursorLine = 0
	e.cursorCol = 0
}

func (e *Editor) HandleInput(data string) {
	if strings.HasPrefix(data, pastePrefix) && strings.HasSuffix(data, pasteSuffix) {
		e.insertString(strings.TrimSuffix(strings.TrimPrefix(data, pastePrefix), pasteSuffix))
		return
	}

	switch ParseKey(data) {
	case "left":
		e.moveLeft()
	case "right":
		e.moveRight()
	case "up":
		e.moveUp()
	case "down":
		e.moveDown()
	case "home", "ctrl+a":
		e.cursorCol = 0
	case "end", "ctrl+e":
		e.cursorCol = len([]rune(e.currentLine()))
	case "backspace":
		e.backspace()
	case "delete":
		e.deleteForward()
	case "ctrl+u":
		e.deleteToStart()
	case "ctrl+k":
		e.deleteToEnd()
	case "ctrl+w":
		e.deleteWordBackward()
	case "shift+enter", "alt+enter":
		e.insertNewline()
	case "enter":
		if e.disableSubmit {
			return
		}
		text := strings.TrimSpace(e.Value())
		if text == "" {
			return
		}
		if e.onSubmit != nil {
			e.onSubmit(e.Value())
		}
	default:
		if printable := DecodePrintable(data); printable != "" {
			e.insertString(printable)
		}
	}
}

func (e *Editor) Render(width int) []string {
	if width <= 0 {
		return nil
	}
	border := strings.Repeat("─", maxInt(1, width))
	border = e.borderColor(border)
	contentWidth := maxInt(1, width-(e.paddingX*2))
	var content []string
	for i, line := range e.lines {
		content = append(content, e.renderLine(line, i == e.cursorLine, contentWidth)...)
	}
	if len(content) == 0 {
		content = []string{cursorMarker}
	}
	lines := []string{border}
	for _, line := range content {
		lines = append(lines, padLine(strings.Repeat(" ", e.paddingX)+line+strings.Repeat(" ", e.paddingX), width, baseStyle.Render))
	}
	lines = append(lines, border)
	return lines
}

func (e *Editor) renderLine(line string, withCursor bool, width int) []string {
	runes := []rune(line)
	cursorCol := e.cursorCol
	if !withCursor || !e.focused {
		cursorCol = -1
	}
	out := []string{}
	current := ""
	currentWidth := 0
	currentIndex := 0

	for i := 0; i <= len(runes); i++ {
		if i == len(runes) {
			if i == cursorCol {
				current += cursorMarker + "\x1b[7m \x1b[0m"
			}
			out = append(out, current)
			break
		}

		r := string(runes[i])
		rw := ansi.StringWidth(r)
		cell := r
		cellWidth := rw
		if i == cursorCol {
			cell = cursorMarker + "\x1b[7m" + r + "\x1b[0m"
		}

		if currentWidth+cellWidth > width && currentIndex > 0 {
			out = append(out, current)
			current = ""
			currentWidth = 0
			currentIndex = 0
		}

		current += cell
		currentWidth += cellWidth
		currentIndex++
	}

	if len(out) == 0 {
		return []string{""}
	}
	return out
}

func (e *Editor) insertString(s string) {
	for s != "" {
		r, size := utf8.DecodeRuneInString(s)
		if r == utf8.RuneError && size == 1 {
			s = s[1:]
			continue
		}
		if r == '\r' {
			s = s[size:]
			continue
		}
		if r == '\n' {
			e.insertNewline()
			s = s[size:]
			continue
		}
		line := []rune(e.currentLine())
		before := string(line[:e.cursorCol])
		after := string(line[e.cursorCol:])
		e.lines[e.cursorLine] = before + string(r) + after
		e.cursorCol++
		s = s[size:]
	}
}

func (e *Editor) insertNewline() {
	line := []rune(e.currentLine())
	before := string(line[:e.cursorCol])
	after := string(line[e.cursorCol:])
	e.lines[e.cursorLine] = before
	tail := append([]string{after}, e.lines[e.cursorLine+1:]...)
	e.lines = append(e.lines[:e.cursorLine+1], tail...)
	e.cursorLine++
	e.cursorCol = 0
}

func (e *Editor) backspace() {
	if e.cursorCol > 0 {
		line := []rune(e.currentLine())
		e.lines[e.cursorLine] = string(append(line[:e.cursorCol-1], line[e.cursorCol:]...))
		e.cursorCol--
		return
	}
	if e.cursorLine == 0 {
		return
	}
	prev := e.lines[e.cursorLine-1]
	cur := e.lines[e.cursorLine]
	e.cursorCol = len([]rune(prev))
	e.lines[e.cursorLine-1] = prev + cur
	e.lines = append(e.lines[:e.cursorLine], e.lines[e.cursorLine+1:]...)
	e.cursorLine--
}

func (e *Editor) deleteForward() {
	line := []rune(e.currentLine())
	if e.cursorCol < len(line) {
		e.lines[e.cursorLine] = string(append(line[:e.cursorCol], line[e.cursorCol+1:]...))
		return
	}
	if e.cursorLine+1 >= len(e.lines) {
		return
	}
	e.lines[e.cursorLine] += e.lines[e.cursorLine+1]
	e.lines = append(e.lines[:e.cursorLine+1], e.lines[e.cursorLine+2:]...)
}

func (e *Editor) deleteToStart() {
	line := []rune(e.currentLine())
	e.lines[e.cursorLine] = string(line[e.cursorCol:])
	e.cursorCol = 0
}

func (e *Editor) deleteToEnd() {
	line := []rune(e.currentLine())
	e.lines[e.cursorLine] = string(line[:e.cursorCol])
}

func (e *Editor) deleteWordBackward() {
	line := []rune(e.currentLine())
	if e.cursorCol == 0 {
		e.backspace()
		return
	}
	start := e.cursorCol
	for start > 0 && unicode.IsSpace(line[start-1]) {
		start--
	}
	for start > 0 && !unicode.IsSpace(line[start-1]) {
		start--
	}
	e.lines[e.cursorLine] = string(line[:start]) + string(line[e.cursorCol:])
	e.cursorCol = start
}

func (e *Editor) moveLeft() {
	if e.cursorCol > 0 {
		e.cursorCol--
		return
	}
	if e.cursorLine > 0 {
		e.cursorLine--
		e.cursorCol = len([]rune(e.currentLine()))
	}
}

func (e *Editor) moveRight() {
	lineLen := len([]rune(e.currentLine()))
	if e.cursorCol < lineLen {
		e.cursorCol++
		return
	}
	if e.cursorLine+1 < len(e.lines) {
		e.cursorLine++
		e.cursorCol = 0
	}
}

func (e *Editor) moveUp() {
	if e.cursorLine == 0 {
		return
	}
	e.cursorLine--
	e.cursorCol = minInt(e.cursorCol, len([]rune(e.currentLine())))
}

func (e *Editor) moveDown() {
	if e.cursorLine+1 >= len(e.lines) {
		return
	}
	e.cursorLine++
	e.cursorCol = minInt(e.cursorCol, len([]rune(e.currentLine())))
}

func (e *Editor) currentLine() string {
	if len(e.lines) == 0 {
		e.lines = []string{""}
	}
	if e.cursorLine < 0 {
		e.cursorLine = 0
	}
	if e.cursorLine >= len(e.lines) {
		e.cursorLine = len(e.lines) - 1
	}
	return e.lines[e.cursorLine]
}
