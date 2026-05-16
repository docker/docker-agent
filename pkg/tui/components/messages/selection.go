package messages

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

// selectionState encapsulates all state related to text selection
type selectionState struct {
	active          bool
	startLine       int
	startCol        int
	endLine         int
	endCol          int
	mouseButtonDown bool
	mouseY          int // Screen Y coordinate for autoscroll

	// Multi-click detection
	lastClickTime time.Time
	lastClickLine int
	lastClickCol  int
	clickCount    int // 1=single, 2=double, 3=triple

	// Debounced copy: incremented on each click, copy only fires if ID matches
	pendingCopyID int
}

// start initializes a new selection at the given position
func (s *selectionState) start(line, col int) {
	s.active = true
	s.mouseButtonDown = true
	s.startLine = line
	s.startCol = col
	s.endLine = line
	s.endCol = col
}

// update updates the end position of the selection
func (s *selectionState) update(line, col int) {
	s.endLine = line
	s.endCol = col
}

// end finalizes the selection and stops mouse tracking
func (s *selectionState) end() {
	s.mouseButtonDown = false
}

// clear resets all selection state
func (s *selectionState) clear() {
	*s = selectionState{}
}

// normalized returns the selection bounds in normalized order (start <= end)
func (s *selectionState) normalized() (startLine, startCol, endLine, endCol int) {
	startLine, startCol = s.startLine, s.startCol
	endLine, endCol = s.endLine, s.endCol

	if startLine > endLine || (startLine == endLine && startCol > endCol) {
		startLine, endLine = endLine, startLine
		startCol, endCol = endCol, startCol
	}
	return startLine, startCol, endLine, endCol
}

// detectClickType records the click and returns the click count (1=single, 2=double, 3=triple)
func (s *selectionState) detectClickType(line, col int) int {
	now := time.Now()
	colDiff := col - s.lastClickCol
	isConsecutive := !s.lastClickTime.IsZero() &&
		now.Sub(s.lastClickTime) < styles.DoubleClickThreshold &&
		line == s.lastClickLine &&
		colDiff >= -1 && colDiff <= 1

	if isConsecutive {
		s.clickCount++
	} else {
		s.clickCount = 1
	}
	s.lastClickTime = now
	s.lastClickLine = line
	s.lastClickCol = col
	return s.clickCount
}

// AutoScrollTickMsg triggers auto-scroll during selection
type AutoScrollTickMsg struct {
	Direction int // -1 for up, 1 for down
}

// DebouncedCopyMsg triggers a debounced copy after multi-click selection
type DebouncedCopyMsg struct {
	ClickID int // Unique identifier to match with current selection state
}

// autoScroll handles automatic scrolling when selecting near viewport edges
func (m *model) autoScroll() tea.Cmd {
	const scrollThreshold = 2
	direction := 0

	// Use stored screen Y coordinate to check if mouse is in autoscroll region
	// mouseToLineCol subtracts 2 for header, so viewport-relative Y is mouseY - 2
	viewportY := max(m.selection.mouseY-2, 0)

	if viewportY < scrollThreshold && m.scrollOffset > 0 {
		// Scroll up - mouse is near top of viewport
		direction = -1
		m.scrollUp()
		// Update endLine to reflect new scroll position
		m.selection.endLine = max(0, m.selection.endLine-1)
	} else if viewportY >= m.height-scrollThreshold && viewportY < m.height {
		// Scroll down - mouse is near bottom of viewport
		maxScrollOffset := max(0, m.totalHeight-m.height)
		if m.scrollOffset < maxScrollOffset {
			direction = 1
			m.scrollDown()
			// Update endLine to reflect new scroll position
			m.selection.endLine++
		}
	}

	if direction == 0 {
		return nil
	}

	return tea.Tick(20*time.Millisecond, func(time.Time) tea.Msg {
		return AutoScrollTickMsg{Direction: direction}
	})
}

// selectWordAt selects the word at the given line and column position
func (m *model) selectWordAt(line, col int) {
	m.ensureAllItemsRendered()
	lines := m.renderedLines
	if line < 0 || line >= len(lines) {
		return
	}

	originalLine := lines[line]
	plainLine := stripBorderChars(ansi.Strip(originalLine))
	if plainLine == "" {
		return
	}

	// Calculate border offset to adjust column position
	borderOffset := runewidth.StringWidth(ansi.Strip(originalLine)) - runewidth.StringWidth(plainLine)
	runes := []rune(plainLine)

	// Convert display column to rune index
	runeIdx := min(max(0, displayWidthToRuneIndex(plainLine, max(0, col-borderOffset))), len(runes)-1)
	if runeIdx < 0 {
		return
	}

	// Find word boundaries - determine if we're on a word or non-word char
	onWordChar := isWordChar(runes[runeIdx])
	startIdx, endIdx := runeIdx, runeIdx

	// Expand to find contiguous characters of the same type
	for startIdx > 0 && isWordChar(runes[startIdx-1]) == onWordChar {
		startIdx--
	}
	for endIdx < len(runes)-1 && isWordChar(runes[endIdx+1]) == onWordChar {
		endIdx++
	}

	// Convert rune indices back to display columns, accounting for border offset
	startCol := runeIndexToDisplayWidth(plainLine, startIdx) + borderOffset
	endCol := runeIndexToDisplayWidth(plainLine, endIdx+1) + borderOffset

	// Set selection
	m.selection.active = true
	m.selection.startLine = line
	m.selection.startCol = startCol
	m.selection.endLine = line
	m.selection.endCol = endCol
	m.selection.mouseButtonDown = false
}

// selectLineAt selects the entire line at the given line position
func (m *model) selectLineAt(line int) {
	m.ensureAllItemsRendered()
	lines := m.renderedLines
	if line < 0 || line >= len(lines) {
		return
	}

	originalLine := lines[line]
	plainLine := ansi.Strip(originalLine)
	trimmedLine := strings.TrimSpace(plainLine)
	if trimmedLine == "" {
		return
	}

	// Find start column: position of first non-whitespace character
	startCol := runewidth.StringWidth(plainLine) - runewidth.StringWidth(strings.TrimLeft(plainLine, " \t"))
	// Find end column: position after last non-whitespace character
	endCol := runewidth.StringWidth(strings.TrimRight(plainLine, " \t"))

	// Set selection to cover only the text content (excluding padding/borders)
	m.selection.active = true
	m.selection.startLine = line
	m.selection.startCol = startCol
	m.selection.endLine = line
	m.selection.endCol = endCol
	m.selection.mouseButtonDown = false
}

// applySelectionHighlight applies selection highlighting to visible lines
func (m *model) applySelectionHighlight(lines []string, viewportStartLine int) []string {
	startLine, startCol, endLine, endCol := m.selection.normalized()

	highlighted := make([]string, len(lines))

	getLineBounds := func(line string) (textStart, textEnd int) {
		plainLine := ansi.Strip(line)
		textStart = runewidth.StringWidth(plainLine) - runewidth.StringWidth(strings.TrimLeft(plainLine, " \t"))
		textEnd = runewidth.StringWidth(strings.TrimRight(plainLine, " \t"))
		return textStart, textEnd
	}

	for i, line := range lines {
		absoluteLine := viewportStartLine + i

		if absoluteLine < startLine || absoluteLine > endLine {
			highlighted[i] = line
			continue
		}

		textStart, textEnd := getLineBounds(line)
		switch {
		case startLine == endLine && absoluteLine == startLine:
			// Single line selection
			highlighted[i] = m.highlightLine(line, startCol, min(textEnd, endCol))
		case absoluteLine == startLine:
			// Start of multi-line selection
			highlighted[i] = m.highlightLine(line, startCol, textEnd)
		case absoluteLine == endLine:
			// End of multi-line selection
			highlighted[i] = m.highlightLine(line, textStart, min(textEnd, endCol))
		default:
			// Middle of multi-line selection
			highlighted[i] = m.highlightLine(line, textStart, textEnd)
		}
	}

	return highlighted
}

// highlightLine applies selection highlighting to a portion of a line
func (m *model) highlightLine(line string, startCol, endCol int) string {
	return styleLineSegment(line, startCol, endCol, styles.SelectionStyle)
}

// clearSelection resets the selection state
func (m *model) clearSelection() {
	m.selection.clear()
}
