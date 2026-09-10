package ui

import (
	"strings"

	"github.com/docker/docker-agent/pkg/history"
)

// rowSpan identifies the slice of the editor buffer [start, end) shown on a
// single visual row.
type rowSpan struct {
	start int
	end   int
}

// Editor is a multi-line text input. The buffer is a flat rune slice with the
// cursor expressed as an index into it; newlines are stored literally so the
// same structure handles single-line prompts and pasted multi-line text. All
// visual wrapping is derived on demand from a given content width.
type Editor struct {
	value  []rune
	cursor int

	placeholder string

	historyStore *history.History
	history      []string
	histIndex    int
	draft        string
}

func NewEditor(placeholder string, historyStore ...*history.History) *Editor {
	e := &Editor{placeholder: placeholder}
	if len(historyStore) > 0 {
		e.historyStore = historyStore[0]
	}
	e.histIndex = len(e.historyMessages())
	return e
}

func (e *Editor) Text() string { return string(e.value) }

func (e *Editor) IsEmpty() bool { return len(e.value) == 0 }

func (e *Editor) Reset() {
	e.value = nil
	e.cursor = 0
	e.histIndex = len(e.historyMessages())
	e.draft = ""
}

func (e *Editor) SetText(s string) {
	e.value = []rune(s)
	e.cursor = len(e.value)
}

func (e *Editor) Insert(runes []rune) {
	if len(runes) == 0 {
		return
	}
	// Normalise newlines so pasted CRLF/CR content stays single-newline.
	cleaned := make([]rune, 0, len(runes))
	for _, r := range runes {
		if r == '\r' {
			continue
		}
		cleaned = append(cleaned, r)
	}
	next := make([]rune, 0, len(e.value)+len(cleaned))
	next = append(next, e.value[:e.cursor]...)
	next = append(next, cleaned...)
	next = append(next, e.value[e.cursor:]...)
	e.value = next
	e.cursor += len(cleaned)
}

func (e *Editor) InsertNewline() { e.Insert([]rune{'\n'}) }

func (e *Editor) Backspace() {
	if e.cursor == 0 {
		return
	}
	e.value = append(e.value[:e.cursor-1], e.value[e.cursor:]...)
	e.cursor--
}

func (e *Editor) DeleteForward() {
	if e.cursor >= len(e.value) {
		return
	}
	e.value = append(e.value[:e.cursor], e.value[e.cursor+1:]...)
}

func (e *Editor) DeleteWordBack() {
	if e.cursor == 0 {
		return
	}
	start := wordStart(e.value, e.cursor)
	e.value = append(e.value[:start], e.value[e.cursor:]...)
	e.cursor = start
}

func (e *Editor) DeleteToLineStart() {
	start := lineStart(e.value, e.cursor)
	e.value = append(e.value[:start], e.value[e.cursor:]...)
	e.cursor = start
}

func (e *Editor) DeleteToLineEnd() {
	end := lineEnd(e.value, e.cursor)
	e.value = append(e.value[:e.cursor], e.value[end:]...)
}

func (e *Editor) MoveLeft() {
	if e.cursor > 0 {
		e.cursor--
	}
}

func (e *Editor) MoveRight() {
	if e.cursor < len(e.value) {
		e.cursor++
	}
}

func (e *Editor) MoveWordLeft()  { e.cursor = wordStart(e.value, e.cursor) }
func (e *Editor) MoveWordRight() { e.cursor = wordEnd(e.value, e.cursor) }
func (e *Editor) MoveLineStart() { e.cursor = lineStart(e.value, e.cursor) }
func (e *Editor) MoveLineEnd()   { e.cursor = lineEnd(e.value, e.cursor) }

// Up moves the cursor one visual row up, preserving the column. It reports
// false when the cursor is already on the first row, letting the caller fall
// back to history navigation.
func (e *Editor) Up(termWidth int) bool {
	width := contentWidth(termWidth)
	rows := e.wrapRows(width)
	row, col := e.cursorPos(rows)
	if row == 0 {
		return false
	}
	e.cursor = e.indexAt(rows, row-1, col)
	return true
}

func (e *Editor) Down(termWidth int) bool {
	width := contentWidth(termWidth)
	rows := e.wrapRows(width)
	row, col := e.cursorPos(rows)
	if row >= len(rows)-1 {
		return false
	}
	e.cursor = e.indexAt(rows, row+1, col)
	return true
}

// RememberHistory records a submitted entry and resets the history cursor.
func (e *Editor) RememberHistory(s string) error {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	if e.historyStore != nil {
		if err := e.historyStore.Add(s); err != nil {
			e.histIndex = len(e.historyStore.Messages)
			e.draft = ""
			return err
		}
	} else if n := len(e.history); n == 0 || e.history[n-1] != s {
		e.history = append(e.history, s)
	}
	e.histIndex = len(e.historyMessages())
	e.draft = ""
	return nil
}

func (e *Editor) historyMessages() []string {
	if e.historyStore != nil {
		return e.historyStore.Messages
	}
	return e.history
}

func (e *Editor) HistoryPrev() {
	historyMessages := e.historyMessages()
	if e.histIndex == 0 {
		return
	}
	if e.histIndex == len(historyMessages) {
		e.draft = e.Text()
	}
	e.histIndex--
	e.SetText(historyMessages[e.histIndex])
}

func (e *Editor) HistoryNext() {
	historyMessages := e.historyMessages()
	if e.histIndex >= len(historyMessages) {
		return
	}
	e.histIndex++
	if e.histIndex == len(historyMessages) {
		e.SetText(e.draft)
		return
	}
	e.SetText(historyMessages[e.histIndex])
}

// Layout renders the editor for the given terminal width, returning one styled
// string per physical row along with the hardware cursor position (row within
// the returned slice, column in terminal cells).
func (e *Editor) Layout(termWidth int) (lines []string, curRow, curCol int) {
	width := contentWidth(termWidth)
	rows := e.wrapRows(width)

	if len(e.value) == 0 {
		line := StAccent().Render(PromptText)
		if e.placeholder != "" {
			line += StPlaceholder().Render(Truncate(e.placeholder, width))
		}
		return []string{line}, 0, PromptWidth
	}

	lines = make([]string, len(rows))
	for i, rs := range rows {
		content := string(e.value[rs.start:rs.end])
		if i == 0 {
			lines[i] = StAccent().Render(PromptText) + content
		} else {
			lines[i] = Continuation + content
		}
	}

	row, col := e.cursorPos(rows)
	return lines, row, col + PromptWidth
}

func (e *Editor) wrapRows(width int) []rowSpan {
	if width < 1 {
		width = 1
	}
	var rows []rowSpan
	start, curWidth, i := 0, 0, 0
	for i < len(e.value) {
		r := e.value[i]
		if r == '\n' {
			rows = append(rows, rowSpan{start, i})
			i++
			start = i
			curWidth = 0
			continue
		}
		w := RuneWidth(r)
		if curWidth+w > width && curWidth > 0 {
			rows = append(rows, rowSpan{start, i})
			start = i
			curWidth = 0
			continue
		}
		curWidth += w
		i++
	}
	rows = append(rows, rowSpan{start, len(e.value)})
	return rows
}

func (e *Editor) cursorPos(rows []rowSpan) (row, col int) {
	row = 0
	for i, rs := range rows {
		if rs.start <= e.cursor {
			row = i
		}
	}
	rs := rows[row]
	for i := rs.start; i < e.cursor && i < len(e.value); i++ {
		col += RuneWidth(e.value[i])
	}
	return row, col
}

func (e *Editor) indexAt(rows []rowSpan, row, col int) int {
	if row < 0 {
		row = 0
	}
	if row > len(rows)-1 {
		row = len(rows) - 1
	}
	rs := rows[row]
	w := 0
	for i := rs.start; i < rs.end; i++ {
		rw := RuneWidth(e.value[i])
		if w+rw > col {
			return i
		}
		w += rw
	}
	return rs.end
}

func contentWidth(termWidth int) int {
	w := termWidth - PromptWidth
	if w < 1 {
		return 1
	}
	return w
}

func isWordRune(r rune) bool {
	return r != ' ' && r != '\t' && r != '\n'
}

func wordStart(v []rune, from int) int {
	i := from
	for i > 0 && !isWordRune(v[i-1]) {
		i--
	}
	for i > 0 && isWordRune(v[i-1]) {
		i--
	}
	return i
}

func wordEnd(v []rune, from int) int {
	i := from
	for i < len(v) && !isWordRune(v[i]) {
		i++
	}
	for i < len(v) && isWordRune(v[i]) {
		i++
	}
	return i
}

func lineStart(v []rune, from int) int {
	i := from
	for i > 0 && v[i-1] != '\n' {
		i--
	}
	return i
}

func lineEnd(v []rune, from int) int {
	i := from
	for i < len(v) && v[i] != '\n' {
		i++
	}
	return i
}
