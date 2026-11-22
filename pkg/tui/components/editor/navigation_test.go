package editor

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/core/input"
)

func TestMultiLineNavigation(t *testing.T) {
	t.Parallel()

	t.Run("left arrow navigation in multi-line content", func(t *testing.T) {
		t.Parallel()

		ta := input.NewArea()
		ta.SetWidth(80)
		ta.SetHeight(10)
		ta.Focus()

		e := &editor{
			textarea:  ta,
			userTyped: true,
		}

		// Set multi-line content
		e.textarea.SetValue("line1\nline2\nline3")
		e.textarea.MoveToEnd() // End of line3

		// Verify initial position
		require.Equal(t, 2, e.textarea.Line(), "should start on line 2 (0-indexed)")
		t.Logf("Initial position - Line: %d, Value: %q", e.textarea.Line(), e.textarea.Value())

		// Press left arrow 5 times to get to beginning of line3
		for i := range 5 {
			_, _ = e.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
			t.Logf("After left %d - Line: %d, LineInfo: %+v", i+1, e.textarea.Line(), e.textarea.LineInfo())
		}

		// Should be at beginning of line3 (still on line 2)
		assert.Equal(t, 2, e.textarea.Line(), "cursor should still be on line 2")

		// One more left should wrap to end of line2
		_, _ = e.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
		t.Logf("After left 6 - Line: %d, LineInfo: %+v", e.textarea.Line(), e.textarea.LineInfo())

		// Should now be on line 1 (line2)
		assert.Equal(t, 1, e.textarea.Line(), "cursor should wrap to line 1")

		// Type 'X' - should insert at end of line2
		_, _ = e.Update(tea.KeyPressMsg{Text: "X"})

		value := e.textarea.Value()
		lines := strings.Split(value, "\n")
		t.Logf("Final value: %q, Lines: %v", value, lines)

		// X should be at end of line2
		assert.Equal(t, "line2X", lines[1], "X should be at end of line2")
	})

	t.Run("right arrow navigation in multi-line content", func(t *testing.T) {
		t.Parallel()

		ta := input.NewArea()
		ta.SetWidth(80)
		ta.SetHeight(10)
		ta.Focus()

		e := &editor{
			textarea:  ta,
			userTyped: true,
		}

		// Set multi-line content
		e.textarea.SetValue("line1\nline2\nline3")
		e.textarea.MoveToBegin() // Beginning of line1

		// Verify initial position
		require.Equal(t, 0, e.textarea.Line(), "should start on line 0")
		t.Logf("Initial position - Line: %d", e.textarea.Line())

		// Press right arrow to move through line1 and into line2
		for i := range 7 { // line1 has 5 chars + newline wraps to line2
			_, _ = e.Update(tea.KeyPressMsg{Code: tea.KeyRight})
			t.Logf("After right %d - Line: %d, LineInfo: %+v", i+1, e.textarea.Line(), e.textarea.LineInfo())
		}

		// Should be on line1 (index 1)
		assert.Equal(t, 1, e.textarea.Line(), "cursor should be on line 1")

		// Type 'X'
		_, _ = e.Update(tea.KeyPressMsg{Text: "X"})

		value := e.textarea.Value()
		lines := strings.Split(value, "\n")
		t.Logf("Final value: %q, Lines: %v", value, lines)

		// X should be on line2
		assert.Contains(t, lines[1], "X", "X should be on line 1 (line2)")
	})

	t.Run("up arrow navigation in multi-line content when userTyped is true", func(t *testing.T) {
		t.Parallel()

		ta := input.NewArea()
		ta.SetWidth(80)
		ta.SetHeight(10)
		ta.Focus()

		e := &editor{
			textarea:  ta,
			userTyped: true, // User has typed, so up should navigate cursor, not history
		}

		// Set multi-line content
		e.textarea.SetValue("line1\nline2\nline3")
		e.textarea.MoveToEnd() // End of line3

		require.Equal(t, 2, e.textarea.Line(), "should start on line 2")
		t.Logf("Initial position - Line: %d", e.textarea.Line())

		// Press up arrow - should move cursor up, not navigate history
		_, _ = e.Update(tea.KeyPressMsg{Code: tea.KeyUp})
		t.Logf("After up - Line: %d, Value: %q", e.textarea.Line(), e.textarea.Value())

		// Should be on line 1 now
		assert.Equal(t, 1, e.textarea.Line(), "cursor should move to line 1")

		// Type 'X'
		_, _ = e.Update(tea.KeyPressMsg{Text: "X"})

		value := e.textarea.Value()
		lines := strings.Split(value, "\n")
		t.Logf("After typing X - Lines: %v", lines)

		// X should be on line 1 (line2), not anywhere else
		assert.Contains(t, lines[1], "X", "X should be on line 1 (line2)")
	})

	t.Run("down arrow navigation in multi-line content when userTyped is true", func(t *testing.T) {
		t.Parallel()

		ta := input.NewArea()
		ta.SetWidth(80)
		ta.SetHeight(10)
		ta.Focus()

		e := &editor{
			textarea:  ta,
			userTyped: true, // User has typed, so down should navigate cursor, not history
		}

		// Set multi-line content
		e.textarea.SetValue("line1\nline2\nline3")
		e.textarea.MoveToBegin() // Start of line1

		require.Equal(t, 0, e.textarea.Line(), "should start on line 0")
		t.Logf("Initial position - Line: %d", e.textarea.Line())

		// Press down arrow - should move cursor down, not navigate history
		_, _ = e.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		t.Logf("After down - Line: %d, Value: %q", e.textarea.Line(), e.textarea.Value())

		// Should be on line 1 now
		assert.Equal(t, 1, e.textarea.Line(), "cursor should move to line 1")

		// Type 'X'
		_, _ = e.Update(tea.KeyPressMsg{Text: "X"})

		value := e.textarea.Value()
		lines := strings.Split(value, "\n")
		t.Logf("After typing X - Lines: %v", lines)

		// X should be on line 1 (line2)
		assert.Contains(t, lines[1], "X", "X should be on line 1 (line2)")
	})

	t.Run("backspace after navigating left in multi-line content", func(t *testing.T) {
		t.Parallel()

		ta := input.NewArea()
		ta.SetWidth(80)
		ta.SetHeight(10)
		ta.Focus()

		e := &editor{
			textarea:  ta,
			userTyped: true,
		}

		// Set multi-line content
		e.textarea.SetValue("line1\nline2\nline3")
		e.textarea.MoveToEnd() // End of line3

		// Move left 3 times: "line3" -> cursor moves:
		// Start: after '3' (position 5)
		// Left 1: after 'e' (position 4)
		// Left 2: after 'n' (position 3)
		// Left 3: after 'i' (position 2), cursor between 'i' and 'n'
		for range 3 {
			_, _ = e.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
		}

		t.Logf("After moving left 3 times - Line: %d, LineInfo: %+v", e.textarea.Line(), e.textarea.LineInfo())

		// Backspace should delete the character BEFORE the cursor, which is 'i'
		_, _ = e.handleGraphemeBackspace()

		value := e.textarea.Value()
		lines := strings.Split(value, "\n")
		t.Logf("After backspace - Line: %d, Value: %q, Lines: %v", e.textarea.Line(), value, lines)

		// Should have deleted 'i' to get "lne3" (cursor was after 'i', so 'i' is deleted)
		assert.Equal(t, "lne3", lines[2], "should have deleted 'i' from line3")
		assert.Equal(t, 2, e.textarea.Line(), "cursor should stay on line 2")

		// Now cursor should be at position 1 (between 'l' and 'n')
		// Type 'X' - should insert at cursor position
		_, _ = e.Update(tea.KeyPressMsg{Text: "X"})

		value = e.textarea.Value()
		lines = strings.Split(value, "\n")
		t.Logf("After typing X - Line: %d, Value: %q, Lines: %v", e.textarea.Line(), value, lines)

		// X should be inserted between 'l' and 'n', giving "lXne3"
		assert.Equal(t, "lXne3", lines[2], "X should be inserted at cursor position")
	})

	t.Run("backspace with wide characters", func(t *testing.T) {
		t.Parallel()

		ta := input.NewArea()
		ta.SetWidth(80)
		ta.SetHeight(10)
		ta.Focus()

		e := &editor{
			textarea:  ta,
			userTyped: true,
		}

		// Set content with wide characters (emoji take 2 columns)
		e.textarea.SetValue("hello😀world")
		e.textarea.MoveToEnd()

		t.Logf("Initial value: %q, Line: %d, LineInfo: %+v", e.textarea.Value(), e.textarea.Line(), e.textarea.LineInfo())

		// Move cursor left 5 times to be before 'world' (after the emoji)
		for range 5 {
			_, _ = e.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
		}

		t.Logf("After moving left 5 times - Line: %d, LineInfo: %+v", e.textarea.Line(), e.textarea.LineInfo())

		// Backspace should delete the emoji
		_, _ = e.handleGraphemeBackspace()

		value := e.textarea.Value()
		t.Logf("After backspace - Value: %q", value)

		// Should have deleted the emoji, result should be "helloworld"
		assert.Equal(t, "helloworld", value, "should have deleted the emoji")
	})

	t.Run("backspace in middle of text with emoji", func(t *testing.T) {
		t.Parallel()

		ta := input.NewArea()
		ta.SetWidth(80)
		ta.SetHeight(10)
		ta.Focus()

		e := &editor{
			textarea:  ta,
			userTyped: true,
		}

		// Set content with emoji: "a😀bc"
		e.textarea.SetValue("a😀bc")
		e.textarea.MoveToEnd()

		t.Logf("Initial value: %q, LineInfo: %+v", e.textarea.Value(), e.textarea.LineInfo())

		// Move cursor left 2 times to be after emoji: "a😀|bc"
		for range 2 {
			_, _ = e.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
		}

		t.Logf("After moving left 2 times - LineInfo: %+v", e.textarea.LineInfo())

		// Backspace should delete the emoji
		_, _ = e.handleGraphemeBackspace()

		value := e.textarea.Value()
		t.Logf("After backspace - Value: %q", value)

		// Should have deleted the emoji, result should be "abc"
		assert.Equal(t, "abc", value, "should have deleted the emoji")
	})

	t.Run("navigation in soft-wrapped text", func(t *testing.T) {
		t.Parallel()

		ta := input.NewArea()
		ta.SetWidth(10) // Narrow width to force wrapping
		ta.SetHeight(10)
		ta.Focus()

		e := &editor{
			textarea:  ta,
			userTyped: true,
		}

		// Set content that will wrap: "123456789012345" (15 chars, wraps at ~10)
		e.textarea.SetValue("123456789012345")
		e.textarea.MoveToEnd()

		t.Logf("Initial value: %q, Line: %d, LineInfo: %+v", e.textarea.Value(), e.textarea.Line(), e.textarea.LineInfo())

		// The text should soft-wrap. Let's verify cursor position
		// and navigation still works correctly.

		// Move cursor left 3 times: from end position to 3 chars before end
		// Cursor moves: "...12345|" -> "...12|345"
		for range 3 {
			_, _ = e.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
		}

		t.Logf("After moving left 3 times - Line: %d, LineInfo: %+v", e.textarea.Line(), e.textarea.LineInfo())

		// Backspace deletes the character before cursor
		// If cursor is at position 12 (after character 12 which is '2'), backspace deletes '2'
		_, _ = e.handleGraphemeBackspace()

		value := e.textarea.Value()
		t.Logf("After backspace - Value: %q", value)

		// Should have deleted the character before cursor position, leaving 14 chars
		assert.Equal(t, "12345678901345", value, "should have deleted character from soft-wrapped text")

		// Type 'X' - inserts at current cursor position
		_, _ = e.Update(tea.KeyPressMsg{Text: "X"})

		value = e.textarea.Value()
		t.Logf("After typing X - Value: %q", value)

		// X should be inserted at current cursor position
		// After deleting '2' at position 11, cursor is at position 11
		// Inserting X gives "12345678901X345"
		assert.Equal(t, "12345678901X345", value, "X should be inserted at cursor position")
	})

	t.Run("up arrow in soft-wrapped text", func(t *testing.T) {
		t.Parallel()

		ta := input.NewArea()
		ta.SetWidth(10) // Narrow width to force wrapping
		ta.SetHeight(10)
		ta.Focus()

		e := &editor{
			textarea:  ta,
			userTyped: true,
		}

		// Set content that will soft-wrap across multiple visual lines
		// This is a single logical line that wraps
		e.textarea.SetValue("123456789012345")
		e.textarea.MoveToEnd()

		t.Logf("Initial - Line: %d, LineInfo: %+v", e.textarea.Line(), e.textarea.LineInfo())

		// Press up - should move within the soft-wrapped visual lines
		_, _ = e.Update(tea.KeyPressMsg{Code: tea.KeyUp})

		t.Logf("After up - Line: %d, LineInfo: %+v", e.textarea.Line(), e.textarea.LineInfo())

		// Should still be on line 0 (single logical line) but at different visual position
		assert.Equal(t, 0, e.textarea.Line(), "should still be on logical line 0")

		// Type 'X'
		_, _ = e.Update(tea.KeyPressMsg{Text: "X"})

		value := e.textarea.Value()
		t.Logf("After typing X - Value: %q", value)

		// X should be inserted somewhere in the first visual row (not at end)
		assert.NotEqual(t, "123456789012345X", value, "X should NOT be at end")
		assert.Contains(t, value, "X", "X should be in the value")
	})

	t.Run("backspace with CJK characters", func(t *testing.T) {
		t.Parallel()

		ta := input.NewArea()
		ta.SetWidth(80)
		ta.SetHeight(10)
		ta.Focus()

		e := &editor{
			textarea:  ta,
			userTyped: true,
		}

		// CJK characters: 中 (U+4E2D) has display width 2 but is 1 UTF-16 code unit
		// This is different from emoji which have width 2 AND 2 UTF-16 code units
		e.textarea.SetValue("a中bc")
		e.textarea.MoveToEnd()

		t.Logf("Initial value: %q, LineInfo: %+v", e.textarea.Value(), e.textarea.LineInfo())

		// Move cursor left 2 times to be after 中: "a中|bc"
		for range 2 {
			_, _ = e.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
		}

		t.Logf("After moving left 2 times - LineInfo: %+v", e.textarea.LineInfo())

		// Backspace should delete the CJK character 中
		_, _ = e.handleGraphemeBackspace()

		value := e.textarea.Value()
		t.Logf("After backspace - Value: %q", value)

		// Should have deleted 中, result should be "abc"
		assert.Equal(t, "abc", value, "should have deleted the CJK character")
	})
}
