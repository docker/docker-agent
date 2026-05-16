package message

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/types"
)

var ansiEscape = regexp.MustCompile("\x1b\\[[0-9;]*m")

func stripANSI(s string) string {
	return ansiEscape.ReplaceAllString(s, "")
}

func TestErrorMessageWrapping(t *testing.T) {
	t.Parallel()

	// Create a long error message that should wrap
	longError := "This is a very long error message that should wrap to multiple lines when the width is constrained. " +
		"It contains enough text to exceed typical terminal widths and demonstrate the wrapping behavior."

	msg := types.Error(longError)
	mv := New(msg, nil)

	// Set a narrow width to force wrapping
	width := 50
	mv.SetSize(width, 0)

	// Render the message
	rendered := mv.View()

	// Verify that the message was rendered
	require.NotEmpty(t, rendered)

	// Verify that the content was wrapped (should have multiple lines)
	lines := strings.Split(rendered, "\n")
	assert.Greater(t, len(lines), 1, "Error message should wrap to multiple lines")

	// Verify each line respects the width constraint (accounting for borders and padding)
	for i, line := range lines {
		// Strip ANSI codes for accurate width calculation
		plainLine := stripANSI(line)
		// Allow some flexibility for borders and padding
		assert.LessOrEqual(t, len(plainLine), width+10, "Line %d exceeds width constraint: %q", i, plainLine)
	}
}

func TestErrorMessageWithShortContent(t *testing.T) {
	t.Parallel()

	shortError := "Short error"
	msg := types.Error(shortError)
	mv := New(msg, nil)

	width := 80
	mv.SetSize(width, 0)

	rendered := mv.View()

	// Verify that the message was rendered
	require.NotEmpty(t, rendered)

	// Verify the content is present in the rendered output
	plainRendered := stripANSI(rendered)
	assert.Contains(t, plainRendered, shortError)
}

func TestErrorMessagePreservesContent(t *testing.T) {
	t.Parallel()

	errorContent := "Error: Failed to connect to database\nConnection timeout after 30 seconds"
	msg := types.Error(errorContent)
	mv := New(msg, nil)

	width := 80
	mv.SetSize(width, 0)

	rendered := mv.View()

	// Verify that the message was rendered
	require.NotEmpty(t, rendered)

	// Verify the essential content is preserved (may be reformatted but words should be there)
	plainRendered := stripANSI(rendered)
	assert.Contains(t, plainRendered, "Failed to connect")
	assert.Contains(t, plainRendered, "database")
	assert.Contains(t, plainRendered, "timeout")
}

func TestPreserveLineBreaks(t *testing.T) {
	t.Parallel()
	const nbsp = "\u00A0"
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "single line unchanged",
			input:    "Hello world",
			expected: "Hello world",
		},
		{
			name:     "two lines preserved",
			input:    "Line one\nLine two",
			expected: "Line one\nLine two",
		},
		{
			name:     "empty line preserved",
			input:    "Para one\n\nPara two",
			expected: "Para one\n\nPara two",
		},
		{
			name:     "trailing newline preserved",
			input:    "Line one\n",
			expected: "Line one\n",
		},
		{
			name:     "multiple lines with indentation preserved as nbsp",
			input:    "Hello\n   indented\nback",
			expected: "Hello\n" + nbsp + nbsp + nbsp + "indented\nback",
		},
		{
			name:     "single line with leading spaces",
			input:    "  indented",
			expected: nbsp + nbsp + "indented",
		},
		{
			name:     "tabs are not converted",
			input:    "\tindented",
			expected: "\tindented",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			result := preserveLineBreaks(tc.input)
			assert.Equal(t, tc.expected, result)
		})
	}
}

func TestPreserveIndentation(t *testing.T) {
	t.Parallel()
	const nbsp = "\u00A0"
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "no indentation",
			input:    "hello",
			expected: "hello",
		},
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "single leading space",
			input:    " hello",
			expected: nbsp + "hello",
		},
		{
			name:     "multiple leading spaces",
			input:    "   hello",
			expected: nbsp + nbsp + nbsp + "hello",
		},
		{
			name:     "only spaces",
			input:    "   ",
			expected: nbsp + nbsp + nbsp,
		},
		{
			name:     "spaces in middle not converted",
			input:    "hello world",
			expected: "hello world",
		},
		{
			name:     "leading spaces with spaces in middle",
			input:    "  hello world",
			expected: nbsp + nbsp + "hello world",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			result := preserveIndentation(tc.input)
			assert.Equal(t, tc.expected, result)
		})
	}
}

func TestWelcomeMessagePreservesLineBreaks(t *testing.T) {
	t.Parallel()

	// Simulate YAML multiline content with | syntax
	welcomeContent := "Welcome!\n   indented line\nregular line"
	msg := types.Welcome(welcomeContent)
	mv := New(msg, nil)

	width := 80
	mv.SetSize(width, 0)

	rendered := mv.View()
	require.NotEmpty(t, rendered)

	// The rendered output should have separate lines (hard breaks preserved)
	lines := strings.Split(rendered, "\n")
	assert.Greater(t, len(lines), 2, "Welcome message should preserve line breaks")

	// Verify indentation is preserved in the output
	plainRendered := stripANSI(rendered)
	assert.Contains(t, plainRendered, "indented")
}

func TestUserMessageCollapsible(t *testing.T) {
	t.Parallel()

	// Create a user message with > 30 lines
	lines := make([]string, 35)
	for i := range 35 {
		lines[i] = fmt.Sprintf("Line %d", i+1)
	}
	content := strings.Join(lines, "\n")

	msg := &types.Message{
		Type:    types.MessageTypeUser,
		Content: content,
	}
	mv := New(msg, nil)
	mv.SetSize(80, 0)

	// Initially, it should not be expanded.
	// It should render 5 lines + indicator
	rendered := mv.View()
	renderedPlain := stripANSI(rendered)

	assert.Contains(t, renderedPlain, "Line 1")
	assert.Contains(t, renderedPlain, "Line 5")
	assert.NotContains(t, renderedPlain, "Line 6")
	assert.Contains(t, renderedPlain, "[+] expand 30 more lines")

	// Test IsToggleLine
	// The height calculation inside IsToggleLine relies on mv.Height(80)
	height := mv.Height(80)
	assert.True(t, mv.IsToggleLine(height-1), "Bottom padding line should be toggleable")
	assert.True(t, mv.IsToggleLine(height-2), "Indicator text line should be toggleable")
	assert.True(t, mv.IsToggleLine(height-3), "Empty line above indicator should be toggleable")
	assert.False(t, mv.IsToggleLine(height-4), "Text content lines should not be toggleable")

	// Toggle it
	mv.Toggle()

	// Render again, should be expanded
	renderedExpanded := mv.View()
	renderedExpandedPlain := stripANSI(renderedExpanded)

	assert.Contains(t, renderedExpandedPlain, "Line 1")
	assert.Contains(t, renderedExpandedPlain, "Line 35")
	assert.Contains(t, renderedExpandedPlain, "[-] collapse")
}

func TestUserMessageNotCollapsible(t *testing.T) {
	t.Parallel()

	// Create a user message with <= 30 lines
	lines := make([]string, 10)
	for i := range 10 {
		lines[i] = fmt.Sprintf("Line %d", i+1)
	}
	msg := &types.Message{
		Type:    types.MessageTypeUser,
		Content: strings.Join(lines, "\n"),
	}
	mv := New(msg, nil)
	mv.SetSize(80, 0)

	renderedPlain := stripANSI(mv.View())

	assert.Contains(t, renderedPlain, "Line 10")
	assert.NotContains(t, renderedPlain, "[+] expand")
	assert.NotContains(t, renderedPlain, "[-] collapse")

	height := mv.Height(80)
	assert.False(t, mv.IsToggleLine(height-1))
}
