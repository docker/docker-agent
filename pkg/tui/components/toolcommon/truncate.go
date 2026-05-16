package toolcommon

import (
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

// TruncateText truncates text to fit within maxWidth, adding an ellipsis if needed.
// Uses a forward-scanning approach for O(n) complexity instead of O(n²).
// Note: lipgloss.Width returns the maximum line width for multi-line strings.
func TruncateText(text string, maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}

	// Fast path: check if text fits without truncation.
	if lipgloss.Width(text) <= maxWidth {
		return text
	}

	if maxWidth == 1 {
		return "…"
	}

	runes := []rune(text)
	end := takeRunesThatFit(runes, 0, maxWidth-1)
	return string(runes[:end]) + "…"
}

func takeRunesThatFit(runes []rune, start, width int) int {
	if width <= 0 {
		if start < len(runes) {
			return start + 1
		}
		return start
	}

	end := start
	currentWidth := 0
	for end < len(runes) {
		w := runeWidth(runes[end])
		if currentWidth+w > width {
			break
		}
		currentWidth += w
		end++
	}

	// Ensure progress.
	if end == start && start < len(runes) {
		end = start + 1
	}

	return end
}

// wrapTextWithIndent wraps text where the first line has a different available width.
// Subsequent lines are indented to align with the tool name badge.
// If text starts with a newline, it's considered pre-formatted and no indent is added.
func wrapTextWithIndent(text string, firstLineWidth, subsequentLineWidth int) string {
	if firstLineWidth <= 0 || subsequentLineWidth <= 0 {
		return text
	}

	// Pre-formatted text (starts with newline) doesn't need additional indentation
	if strings.HasPrefix(text, "\n") {
		return text
	}

	// Fast path: single line that fits in first line width
	if !strings.Contains(text, "\n") && lipgloss.Width(text) <= firstLineWidth {
		return text
	}

	indent := strings.Repeat(" ", styles.ToolCompletedIcon.GetMarginLeft())
	var result strings.Builder
	isFirstLine := true

	for inputLine := range strings.SplitSeq(text, "\n") {
		// Determine width and prefix for current line
		width := subsequentLineWidth
		prefix := indent
		if isFirstLine {
			width = firstLineWidth
			prefix = ""
			isFirstLine = false
		}

		// Empty line - just add newline + indent
		if inputLine == "" {
			if result.Len() > 0 {
				result.WriteByte('\n')
			}
			result.WriteString(indent)
			continue
		}

		// Line fits without wrapping
		if lipgloss.Width(inputLine) <= width {
			if result.Len() > 0 {
				result.WriteByte('\n')
			}
			result.WriteString(prefix)
			result.WriteString(inputLine)
			continue
		}

		// Need to wrap this line
		runes := []rune(inputLine)
		start := 0
		for start < len(runes) {
			// After first chunk, use subsequent width/indent
			if start > 0 {
				width = subsequentLineWidth
				prefix = indent
			}

			end := takeRunesThatFit(runes, start, width)

			if result.Len() > 0 {
				result.WriteByte('\n')
			}
			result.WriteString(prefix)
			result.WriteString(string(runes[start:end]))
			start = end
		}
	}

	return result.String()
}

// WrapLinesWords wraps text to fit within the given width, preferring to break
// at word boundaries (spaces). If a single word exceeds the width, it falls
// back to splitting at rune boundaries.
func WrapLinesWords(text string, width int) []string {
	if width <= 0 {
		return strings.Split(text, "\n")
	}

	var lines []string
	for inputLine := range strings.SplitSeq(text, "\n") {
		if lipgloss.Width(inputLine) <= width {
			lines = append(lines, inputLine)
			continue
		}

		words := strings.Fields(inputLine)
		if len(words) == 0 {
			lines = append(lines, inputLine)
			continue
		}

		var current strings.Builder
		currentWidth := 0

		for _, word := range words {
			wWidth := lipgloss.Width(word)

			// Word itself exceeds width — split it at rune boundaries
			if wWidth > width {
				if currentWidth > 0 {
					lines = append(lines, current.String())
					current.Reset()
					currentWidth = 0
				}
				runes := []rune(word)
				start := 0
				for start < len(runes) {
					end := takeRunesThatFit(runes, start, width)
					lines = append(lines, string(runes[start:end]))
					start = end
				}
				continue
			}

			needed := wWidth
			if currentWidth > 0 {
				needed++ // space separator
			}

			if currentWidth+needed > width {
				lines = append(lines, current.String())
				current.Reset()
				currentWidth = 0
			}

			if currentWidth > 0 {
				current.WriteByte(' ')
				currentWidth++
			}
			current.WriteString(word)
			currentWidth += wWidth
		}

		if current.Len() > 0 {
			lines = append(lines, current.String())
		}
	}
	return lines
}

// WrapLines wraps text to fit within the given width.
// Each line that exceeds the width is split at rune boundaries.
func WrapLines(text string, width int) []string {
	if width <= 0 {
		return strings.Split(text, "\n")
	}

	var lines []string
	for inputLine := range strings.SplitSeq(text, "\n") {
		// Fast path: if line fits, add directly without rune conversion
		if lipgloss.Width(inputLine) <= width {
			lines = append(lines, inputLine)
			continue
		}

		// Convert to runes once for the entire line.
		runes := []rune(inputLine)
		start := 0

		for start < len(runes) {
			end := takeRunesThatFit(runes, start, width)
			lines = append(lines, string(runes[start:end]))
			start = end
		}
	}
	return lines
}
