package messages

import (
	"slices"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/atotto/clipboard"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"

	"github.com/docker/docker-agent/pkg/tui/components/markdown"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/types"
)

var (
	clipboardMu    sync.RWMutex
	writeClipboard = clipboard.WriteAll
)

// SetClipboardWriterForTest replaces the system clipboard writer and returns a
// restore function. It is intended for black-box TUI tests that need to assert
// copy behavior without touching the developer or CI machine's real clipboard.
func SetClipboardWriterForTest(fn func(string) error) func() {
	clipboardMu.Lock()
	defer clipboardMu.Unlock()
	prev := writeClipboard
	if fn == nil {
		writeClipboard = clipboard.WriteAll
	} else {
		writeClipboard = fn
	}

	return func() {
		clipboardMu.Lock()
		defer clipboardMu.Unlock()
		writeClipboard = prev
	}
}

func clipboardWriter() func(string) error {
	clipboardMu.RLock()
	defer clipboardMu.RUnlock()
	return writeClipboard
}

// boxDrawingChars contains Unicode box-drawing characters used by lipgloss borders.
// These need to be stripped when copying text to clipboard.
var boxDrawingChars = map[rune]bool{
	// Thick border characters
	'┃': true, '━': true, '┏': true, '┓': true, '┗': true, '┛': true,
	// Normal border characters
	'│': true, '─': true, '┌': true, '┐': true, '└': true, '┘': true,
	// Double border characters
	'║': true, '═': true, '╔': true, '╗': true, '╚': true, '╝': true,
	// Rounded border characters
	'╭': true, '╮': true, '╯': true, '╰': true,
	// Block border characters
	'█': true, '▀': true, '▄': true,
	// Additional box-drawing characters that might appear
	'┣': true, '┫': true, '┳': true, '┻': true, '╋': true,
	'├': true, '┤': true, '┬': true, '┴': true, '┼': true,
	'╠': true, '╣': true, '╦': true, '╩': true, '╬': true,
}

// stripBorderChars removes box-drawing characters from text.
// This is used when copying selected text to clipboard to avoid
// including visual border decorations in the copied content.
func stripBorderChars(s string) string {
	var result strings.Builder
	result.Grow(len(s))
	for _, r := range s {
		if !boxDrawingChars[r] {
			result.WriteRune(r)
		}
	}
	return result.String()
}

// isWordChar returns true if the rune is a word character (letter, digit, or underscore)
func isWordChar(r rune) bool {
	return (r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9') ||
		r == '_' ||
		r >= 0x80 // Include non-ASCII characters (unicode letters, etc.)
}

// displayWidthToRuneIndex converts a display width to a rune index
func displayWidthToRuneIndex(s string, targetWidth int) int {
	if targetWidth <= 0 {
		return 0
	}

	runes := []rune(s)
	currentWidth := 0

	for i, r := range runes {
		if currentWidth >= targetWidth {
			return i
		}
		currentWidth += runewidth.RuneWidth(r)
	}

	return len(runes)
}

// runeIndexToDisplayWidth converts a rune index to display width
func runeIndexToDisplayWidth(s string, runeIdx int) int {
	runes := []rune(s)
	if runeIdx > len(runes) {
		runeIdx = len(runes)
	}
	width := 0
	for i := range runeIdx {
		width += runewidth.RuneWidth(runes[i])
	}
	return width
}

// uiAffordanceLines lists the purely decorative click affordances (copy,
// edit, retry labels, collapse indicator) that can appear on their own line
// in rendered messages. Built as a slice because some labels share the same
// text (message-level and code-block copy), which a switch would reject as
// duplicate cases.
//
// Matching is whole-line (after trimming): a drag that covers only part of an
// affordance row keeps the fragment, and a content line that exactly equals a
// label is dropped. Both are accepted trade-offs of text matching; a
// structural fix would record affordance line indices at render time.
var uiAffordanceLines = []string{
	types.MessageCopyLabel,
	markdown.CodeBlockCopyIcon,
	types.UserMessageEditLabel,
	// Editable user messages render both labels on one action row.
	types.UserMessageEditLabel + types.MessageActionSeparator + types.MessageCopyLabel,
	types.ErrorRetryLabel,
	"[-] collapse",
}

// isUIAffordanceLine reports whether a selected line, once trimmed, is one of
// the purely decorative click affordances. Those lines are UI chrome, not
// message content, so they are dropped from clipboard copies.
func isUIAffordanceLine(trimmed string) bool {
	if trimmed == "" {
		return false
	}
	if slices.Contains(uiAffordanceLines, trimmed) {
		return true
	}
	return strings.HasPrefix(trimmed, "[+] expand ") && strings.HasSuffix(trimmed, " more lines")
}

// extractSelectedText extracts the currently selected text from rendered content
func (m *model) extractSelectedText() string {
	if !m.selection.active {
		return ""
	}

	m.ensureAllItemsRendered()
	startLine, startCol, endLine, endCol := m.selection.normalized()

	if startLine < 0 || startLine >= m.totalHeight {
		return ""
	}
	if endLine >= m.totalHeight {
		endLine = m.totalHeight - 1
	}

	var selected []string
	for i := startLine; i <= endLine && i < m.totalHeight; i++ {
		originalLine := m.renderedLine(i)
		// Strip ANSI codes first to get the displayed text with borders
		plainLine := ansi.Strip(originalLine)
		// Strip border characters to get the actual text content
		line := stripBorderChars(plainLine)
		runes := []rune(line)

		// Map visual column positions from the plain line (with borders) to the
		// stripped line (without borders) by tracking which runes correspond to
		// which visual columns
		visualToRune := make(map[int]int)
		visualCol := 0
		lineRuneIdx := 0
		for _, r := range plainLine {
			if !boxDrawingChars[r] {
				// This rune is kept in the stripped line
				visualToRune[visualCol] = lineRuneIdx
				lineRuneIdx++
			}
			visualCol += runewidth.RuneWidth(r)
		}

		// Find the closest rune index for the start and end columns
		lineWidth := visualCol
		startRuneIdx := findClosestRuneIndex(visualToRune, startCol, len(runes), lineWidth)
		endRuneIdx := findClosestRuneIndex(visualToRune, endCol, len(runes), lineWidth)

		var lineText string
		switch i {
		case startLine:
			if startLine == endLine {
				if startRuneIdx < len(runes) && startRuneIdx < endRuneIdx {
					lineText = string(runes[startRuneIdx:endRuneIdx])
				}
				break
			}
			// First line: from startCol to end
			if startRuneIdx < len(runes) {
				lineText = string(runes[startRuneIdx:])
			}
		case endLine:
			// Last line: from start to endCol
			lineText = string(runes[:endRuneIdx])
		default:
			// Middle lines: entire line
			lineText = line
		}

		// Keep leading whitespace (indentation) but drop the width padding
		// that rendered lines carry on the right.
		lineText = strings.TrimRight(lineText, " \t")
		if isUIAffordanceLine(strings.TrimSpace(lineText)) {
			continue
		}
		selected = append(selected, lineText)
	}

	return cleanSelectedLines(selected)
}

// cleanSelectedLines normalizes extracted selection lines for the clipboard:
// blank lines at both ends are dropped (message padding, separators) and the
// common leading whitespace shared by all non-blank lines is removed, so text
// loses the message envelope's padding but keeps its relative indentation
// (essential when copying code).
func cleanSelectedLines(lines []string) string {
	start, end := 0, len(lines)
	for start < end && lines[start] == "" {
		start++
	}
	for end > start && lines[end-1] == "" {
		end--
	}
	lines = lines[start:end]
	if len(lines) == 0 {
		return ""
	}

	indent := -1
	for _, line := range lines {
		if line == "" {
			continue
		}
		n := len(line) - len(strings.TrimLeft(line, " "))
		if indent < 0 || n < indent {
			indent = n
		}
	}
	if indent <= 0 {
		return strings.Join(lines, "\n")
	}

	out := make([]string, len(lines))
	for i, line := range lines {
		if line != "" {
			out[i] = line[indent:]
		}
	}
	return strings.Join(out, "\n")
}

// findClosestRuneIndex finds the rune index for a given visual column,
// or the closest next rune if the exact column doesn't exist.
// Columns at or beyond the end of the line map to one past the last rune, so
// a selection dragged past the end of an unpadded line still includes its
// last character (slicing treats the end index as exclusive).
func findClosestRuneIndex(visualToRune map[int]int, visualCol, maxRunes, lineWidth int) int {
	// Try exact match first
	if runeIdx, ok := visualToRune[visualCol]; ok {
		return runeIdx
	}

	if visualCol >= lineWidth {
		return maxRunes
	}

	// Find the next available rune index after the visual column
	for col := visualCol + 1; col <= visualCol+10; col++ {
		if runeIdx, ok := visualToRune[col]; ok {
			return runeIdx
		}
	}

	// Find the previous available rune index
	for col := visualCol - 1; col >= 0; col-- {
		if runeIdx, ok := visualToRune[col]; ok {
			return runeIdx
		}
	}

	// Fallback: return the last rune index
	return maxRunes
}

// copySelectionToClipboard copies the currently selected text to clipboard
func (m *model) copySelectionToClipboard() tea.Cmd {
	if !m.selection.active {
		return nil
	}

	selectedText := m.extractSelectedText()
	if selectedText == "" {
		return nil
	}

	return copyTextToClipboard(selectedText)
}

// copySelectedMessageToClipboard copies the content of the selected message to clipboard
func (m *model) copySelectedMessageToClipboard() tea.Cmd {
	if m.selectedMessageIndex < 0 || m.selectedMessageIndex >= len(m.messages) {
		return nil
	}

	msg := m.messages[m.selectedMessageIndex]
	content := msg.Content

	if content == "" {
		return nil
	}

	return copyTextToClipboard(content)
}

// copyTextToClipboard copies text to the system clipboard and confirms with
// a toast. Copy buttons that flash an inline "copied" label must use
// copyTextToClipboardSilent instead to avoid double feedback.
func copyTextToClipboard(text string) tea.Cmd {
	return tea.Sequence(
		copyTextToClipboardSilent(text),
		notification.SuccessCmd("Text copied to clipboard."),
	)
}

// copyTextToClipboardSilent copies text to the system clipboard without a
// toast notification.
func copyTextToClipboardSilent(text string) tea.Cmd {
	return tea.Sequence(
		func() tea.Msg {
			_ = clipboardWriter()(text)
			return nil
		},
		tea.SetClipboard(text),
	)
}

// scheduleDebouncedCopy schedules a copy after a delay, allowing triple-click to cancel it.
func (m *model) scheduleDebouncedCopy() tea.Cmd {
	m.selection.pendingCopyID++
	copyID := m.selection.pendingCopyID
	return tea.Tick(400*time.Millisecond, func(time.Time) tea.Msg {
		return DebouncedCopyMsg{ClickID: copyID}
	})
}

// handleDebouncedCopy executes copy only if no subsequent click invalidated it.
func (m *model) handleDebouncedCopy(msg DebouncedCopyMsg) tea.Cmd {
	if msg.ClickID == m.selection.pendingCopyID {
		return m.copySelectionToClipboard()
	}
	return nil
}
