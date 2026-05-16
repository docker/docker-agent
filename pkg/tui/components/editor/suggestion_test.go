package editor

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/history"
)

func TestApplySuggestionOverlay(t *testing.T) {
	tests := []struct {
		name         string
		suggestion   string
		textareaText string
		shouldPanic  bool
	}{
		{
			name:         "single line suggestion",
			suggestion:   "complete this",
			textareaText: "hello ",
			shouldPanic:  false,
		},
		{
			name:         "two line suggestion",
			suggestion:   "line 1\nline 2",
			textareaText: "start ",
			shouldPanic:  false,
		},
		{
			name:         "three line suggestion",
			suggestion:   "line 1\nline 2\nline 3",
			textareaText: "start ",
			shouldPanic:  false,
		},
		{
			name:         "many line suggestion",
			suggestion:   "line 1\nline 2\nline 3\nline 4\nline 5\nline 6\nline 7",
			textareaText: "start ",
			shouldPanic:  false,
		},
		{
			name:         "empty suggestion",
			suggestion:   "",
			textareaText: "hello",
			shouldPanic:  false,
		},
		{
			name:         "newline only suggestion",
			suggestion:   "\n",
			textareaText: "hello",
			shouldPanic:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Create a simple editor for testing
			hist, _ := history.New("")
			e := New(hist).(*editor)

			// Set up textarea with test content
			e.textarea.SetValue(tt.textareaText)
			e.textarea.SetWidth(80)
			e.textarea.SetHeight(10)

			// Set up suggestion
			e.suggestion = tt.suggestion
			e.hasSuggestion = true

			// Create a simple base view
			baseView := e.textarea.View()

			// Test the function
			if tt.shouldPanic {
				assert.Panics(t, func() {
					e.applySuggestionOverlay(baseView)
				})
			} else {
				assert.NotPanics(t, func() {
					result := e.applySuggestionOverlay(baseView)
					// Just verify we get some output
					assert.NotEmpty(t, result)
				})
			}
		})
	}
}

func TestApplySuggestionOverlayScrolledView(t *testing.T) {
	t.Parallel()

	hist, _ := history.New("")
	e := New(hist).(*editor)

	// Set up a multi-line textarea that's scrolled
	content := "line 1\nline 2\nline 3\nline 4\nline 5\nline 6\nline 7\nline 8\nline 9\nline 10\ncurrent"
	e.textarea.SetValue(content)
	e.textarea.SetWidth(80)
	e.textarea.SetHeight(3) // Small height to force scrolling

	// Move cursor to end to scroll to bottom
	e.textarea.MoveToEnd()

	// Set suggestion
	e.suggestion = "suggestion text"
	e.hasSuggestion = true

	baseView := e.textarea.View()

	// Should not panic even with scrolled content
	require.NotPanics(t, func() {
		result := e.applySuggestionOverlay(baseView)
		assert.NotEmpty(t, result)
	})
}

// TestApplySuggestionOverlayWithVariousLineCount tests suggestion overlay positioning
// with 0, 1, 2, 3, and 4 lines of text in the editor.
func TestApplySuggestionOverlayWithVariousLineCount(t *testing.T) {
	tests := []struct {
		name            string
		textareaText    string
		suggestion      string
		expectedLine    int    // Which visual line should contain the suggestion
		expectAfterText string // Text that should appear BEFORE the suggestion on that line (empty if at start)
	}{
		{
			name:            "0 lines - empty editor",
			textareaText:    "",
			suggestion:      "SUGG",
			expectedLine:    0,
			expectAfterText: "", // Suggestion at column 0 (value is empty)
		},
		{
			name:            "1 line - single char",
			textareaText:    "a",
			suggestion:      "SUGG",
			expectedLine:    0,
			expectAfterText: "a",
		},
		{
			name:            "1 line - with text",
			textareaText:    "hello world",
			suggestion:      "SUGG",
			expectedLine:    0,
			expectAfterText: "hello world",
		},
		{
			name:            "1 line ending with newline - cursor on empty second line",
			textareaText:    "hello\n",
			suggestion:      "SUGG",
			expectedLine:    1,  // Cursor is on line 1 (empty line after "hello")
			expectAfterText: "", // Suggestion at column 0 on empty line
		},
		{
			name:            "2 lines",
			textareaText:    "line1\nline2",
			suggestion:      "SUGG",
			expectedLine:    1,
			expectAfterText: "line2",
		},
		{
			name:            "2 lines ending with newline",
			textareaText:    "line1\nline2\n",
			suggestion:      "SUGG",
			expectedLine:    2,  // Cursor is on line 2 (empty line after "line2")
			expectAfterText: "", // Suggestion at column 0 on empty line
		},
		{
			name:            "3 lines",
			textareaText:    "a\nb\nc",
			suggestion:      "SUGG",
			expectedLine:    2,
			expectAfterText: "c",
		},
		{
			name:            "4 lines",
			textareaText:    "a\nb\nc\nd",
			suggestion:      "SUGG",
			expectedLine:    3,
			expectAfterText: "d",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			hist, _ := history.New("")
			e := New(hist).(*editor)

			e.textarea.SetValue(tt.textareaText)
			e.textarea.SetWidth(80)
			e.textarea.SetHeight(10)
			e.textarea.MoveToEnd()

			e.suggestion = tt.suggestion
			e.hasSuggestion = true

			baseView := e.textarea.View()
			result := e.applySuggestionOverlay(baseView)

			// Parse the result and verify suggestion position
			lines := strings.Split(result, "\n")
			require.Greater(t, len(lines), tt.expectedLine,
				"result should have at least %d lines", tt.expectedLine+1)

			// Get the line where we expect the suggestion
			targetLine := stripANSI(lines[tt.expectedLine])

			// The suggestion should appear on this line (first char has cursor style, rest is ghost)
			// Check that the suggestion text appears on the target line
			assert.Contains(t, targetLine, tt.suggestion,
				"suggestion %q should appear on line %d", tt.suggestion, tt.expectedLine)

			// Verify the suggestion appears immediately after the expected text (no gap)
			if tt.expectAfterText != "" {
				expectedContent := tt.expectAfterText + tt.suggestion
				assert.Contains(t, targetLine, expectedContent,
					"line should contain %q, got %q", expectedContent, targetLine)
			}
		})
	}
}

// TestApplySuggestionOverlayWithMultiLineSuggestion tests that multi-line suggestions
// are rendered correctly starting from the target line.
func TestApplySuggestionOverlayWithMultiLineSuggestion(t *testing.T) {
	tests := []struct {
		name           string
		textareaText   string
		suggestion     string
		expectedLine   int // First line of suggestion
		editorHeight   int
		checkFirstLine bool // Whether to verify the first line of suggestion
	}{
		{
			name:           "2-line suggestion on single line text",
			textareaText:   "hello",
			suggestion:     " world\nmore text",
			expectedLine:   0,
			editorHeight:   10,
			checkFirstLine: true,
		},
		{
			name:           "3-line suggestion on 2 line text",
			textareaText:   "line1\nline2",
			suggestion:     " end\nline3\nline4",
			expectedLine:   1,
			editorHeight:   10,
			checkFirstLine: true,
		},
		{
			name:           "multi-line suggestion with scrolled view",
			textareaText:   "a\nb\nc\nd\ne",
			suggestion:     "f\ng\nh",
			expectedLine:   2, // Only showing last 3 lines due to height=3
			editorHeight:   3,
			checkFirstLine: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			hist, _ := history.New("")
			e := New(hist).(*editor)

			e.textarea.SetValue(tt.textareaText)
			e.textarea.SetWidth(80)
			e.textarea.SetHeight(tt.editorHeight)
			e.textarea.MoveToEnd()

			e.suggestion = tt.suggestion
			e.hasSuggestion = true

			baseView := e.textarea.View()
			result := e.applySuggestionOverlay(baseView)

			// The function should not panic
			require.NotEmpty(t, result)

			// Verify at least the first line of suggestion is present
			if tt.checkFirstLine {
				lines := strings.Split(result, "\n")
				require.Greater(t, len(lines), tt.expectedLine)

				firstSuggestionPart := strings.Split(tt.suggestion, "\n")[0]
				targetLine := stripANSI(lines[tt.expectedLine])
				assert.Contains(t, targetLine, firstSuggestionPart,
					"first part of suggestion %q should appear on line %d, got %q",
					firstSuggestionPart, tt.expectedLine, targetLine)
			}
		})
	}
}

// TestIsCursorAtEnd tests the isCursorAtEnd helper function.
func TestIsCursorAtEnd(t *testing.T) {
	tests := []struct {
		name       string
		value      string
		width      int             // Editor width (for soft-wrap testing)
		moveCursor func(e *editor) // How to position cursor after setting value
		want       bool
	}{
		{
			name:       "empty editor",
			value:      "",
			width:      80,
			moveCursor: nil,
			want:       true,
		},
		{
			name:       "cursor at end of single line",
			value:      "hello",
			width:      80,
			moveCursor: func(e *editor) { e.textarea.MoveToEnd() },
			want:       true,
		},
		{
			name:  "cursor not at end - moved left",
			value: "hello",
			width: 80,
			moveCursor: func(e *editor) {
				e.textarea.MoveToEnd()
				e.textarea.SetCursorColumn(2) // Move to position 2
			},
			want: false,
		},
		{
			name:       "cursor at end of multi-line",
			value:      "line1\nline2",
			width:      80,
			moveCursor: func(e *editor) { e.textarea.MoveToEnd() },
			want:       true,
		},
		{
			name:  "cursor on first line of multi-line",
			value: "line1\nline2",
			width: 80,
			moveCursor: func(e *editor) {
				e.textarea.MoveToBegin() // Move to start (first line)
			},
			want: false,
		},
		{
			name:  "cursor at start of last line",
			value: "line1\nline2",
			width: 80,
			moveCursor: func(e *editor) {
				e.textarea.MoveToEnd()
				e.textarea.SetCursorColumn(0) // Move to start of last line
			},
			want: false,
		},
		{
			name:       "soft-wrapped text - cursor at end",
			value:      "this is a long line that will wrap to multiple visual lines",
			width:      20, // Narrow width to force wrapping
			moveCursor: func(e *editor) { e.textarea.MoveToEnd() },
			want:       true,
		},
		{
			name:  "soft-wrapped text - cursor in middle",
			value: "this is a long line that will wrap to multiple visual lines",
			width: 20,
			moveCursor: func(e *editor) {
				e.textarea.MoveToEnd()
				e.textarea.SetCursorColumn(5) // Move to middle of last visual line
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			hist, _ := history.New("")
			e := New(hist).(*editor)

			e.textarea.SetValue(tt.value)
			e.textarea.SetWidth(tt.width)
			e.textarea.SetHeight(10)

			if tt.moveCursor != nil {
				tt.moveCursor(e)
			}

			got := e.isCursorAtEnd()
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestExtractLineText tests the extractLineText helper function.
func TestExtractLineText(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		prompt   string
		expected string
	}{
		{
			name:     "plain text no prompt",
			line:     "hello world       ",
			prompt:   "",
			expected: "hello world",
		},
		{
			name:     "with ANSI codes",
			line:     "\x1b[31mred text\x1b[0m       ",
			prompt:   "",
			expected: "red text",
		},
		{
			name:     "with prompt",
			line:     "> hello       ",
			prompt:   "> ",
			expected: "hello",
		},
		{
			name:     "empty line",
			line:     "          ",
			prompt:   "",
			expected: "",
		},
		{
			name:     "only prompt",
			line:     "> ",
			prompt:   "> ",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := extractLineText(tt.line, tt.prompt)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// TestMultiLineSuggestionWithSmallEditor verifies that multi-line suggestions
// render correctly even when the editor view is smaller than the suggestion.
func TestMultiLineSuggestionWithSmallEditor(t *testing.T) {
	t.Parallel()

	hist, _ := history.New("")
	e := New(hist).(*editor)

	// Set up editor with minimal height
	e.textarea.SetValue("A")
	e.textarea.SetWidth(80)
	e.textarea.SetHeight(1) // Only 1 line visible
	e.textarea.MoveToEnd()

	// Set a multi-line suggestion
	e.suggestion = "nything\nline2\nline3"
	e.hasSuggestion = true

	baseView := e.textarea.View()
	result := e.applySuggestionOverlay(baseView)

	// The result should contain all suggestion lines
	resultLines := strings.Split(result, "\n")

	// With height=1, base view has 1 line, but result should have 3 lines
	// because lipgloss canvas extends the output for overlay content
	require.GreaterOrEqual(t, len(resultLines), 3, "result should have at least 3 lines for multi-line suggestion")

	// Check that each suggestion line is present
	assert.Contains(t, stripANSI(resultLines[0]), "Anything", "first line should contain merged text")
	assert.Contains(t, stripANSI(resultLines[1]), "line2", "second line should contain line2")
	assert.Contains(t, stripANSI(resultLines[2]), "line3", "third line should contain line3")
}

// TestLongSuggestionWrapping verifies that long suggestions (without newlines)
// are properly wrapped to fit within the editor width using textarea's word-wrap.
func TestLongSuggestionWrapping(t *testing.T) {
	t.Parallel()

	hist, _ := history.New("")
	e := New(hist).(*editor)

	// Set up editor with narrow width
	e.textarea.SetValue("L")
	e.textarea.SetWidth(20) // Very narrow
	e.textarea.SetHeight(5)
	e.textarea.MoveToEnd()

	// Set a long suggestion that should wrap (more than 19 chars available after "L")
	e.suggestion = "ook at the last version of the ACP spec"
	e.hasSuggestion = true

	baseView := e.textarea.View()
	result := e.applySuggestionOverlay(baseView)

	resultLines := strings.Split(result, "\n")

	// The suggestion should be wrapped across multiple lines
	require.GreaterOrEqual(t, len(resultLines), 3, "long suggestion should wrap to at least 3 lines")

	// First line should have "L" + start of suggestion
	line0 := stripANSI(resultLines[0])
	assert.True(t, strings.HasPrefix(line0, "L"), "first line should start with L")
	assert.Contains(t, line0, "ook", "first line should contain start of suggestion")
}
