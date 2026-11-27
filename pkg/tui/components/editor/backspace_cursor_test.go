package editor

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/tui/core/input"
)

func splitLines(s string) []string {
	return strings.Split(s, "\n")
}

func TestBackspaceCursorPosition(t *testing.T) {
	t.Parallel()

	t.Run("backspace on middle line keeps cursor on same line", func(t *testing.T) {
		t.Parallel()

		ta := input.NewArea()
		ta.SetWidth(80)
		ta.SetHeight(10)
		ta.Focus()

		e := &editor{
			textarea:  ta,
			userTyped: true,
		}

		// Set multi-line content: "line1\nline2\nline3"
		e.textarea.SetValue("line1\nline2\nline3")
		e.textarea.MoveToEnd() // End of line3

		// Move up to line 2
		_, _ = e.Update(tea.KeyPressMsg{Code: tea.KeyUp})

		t.Logf("Before backspace - Line: %d, Value: %q", e.textarea.Line(), e.textarea.Value())
		assert.Equal(t, 1, e.textarea.Line(), "should be on line 1")

		// Do backspace (using our grapheme-aware handler)
		_, _ = e.handleGraphemeBackspace()

		value := e.textarea.Value()
		t.Logf("After backspace - Line: %d, Value: %q", e.textarea.Line(), value)

		// Cursor should still be on line 1
		assert.Equal(t, 1, e.textarea.Line(), "cursor should stay on line 1")

		// Type X
		_, _ = e.Update(tea.KeyPressMsg{Text: "X"})

		value = e.textarea.Value()
		t.Logf("After typing X - Line: %d, Value: %q", e.textarea.Line(), value)

		lines := splitLines(value)
		t.Logf("Lines: %v", lines)

		// X should be on line 1
		assert.Contains(t, lines[1], "X", "X should be on line 1 (line2)")
	})

	t.Run("multiple backspaces then type", func(t *testing.T) {
		t.Parallel()

		ta := input.NewArea()
		ta.SetWidth(80)
		ta.SetHeight(10)
		ta.Focus()

		e := &editor{
			textarea:  ta,
			userTyped: true,
		}

		e.textarea.SetValue("AAA\nBBB\nCCC")
		e.textarea.MoveToEnd()

		// Go to line 1 (BBB)
		_, _ = e.Update(tea.KeyPressMsg{Code: tea.KeyUp})
		t.Logf("After up - Line: %d", e.textarea.Line())

		// Delete all of BBB
		for i := range 3 {
			_, _ = e.handleGraphemeBackspace()
			t.Logf("After backspace %d - Line: %d, Value: %q", i+1, e.textarea.Line(), e.textarea.Value())
		}

		// Type XXX
		_, _ = e.Update(tea.KeyPressMsg{Text: "X"})
		_, _ = e.Update(tea.KeyPressMsg{Text: "X"})
		_, _ = e.Update(tea.KeyPressMsg{Text: "X"})

		value := e.textarea.Value()
		t.Logf("Final - Line: %d, Value: %q", e.textarea.Line(), value)

		lines := splitLines(value)
		assert.Equal(t, "AAA", lines[0])
		assert.Equal(t, "XXX", lines[1], "XXX should replace BBB on line 1")
		assert.Equal(t, "CCC", lines[2])
	})
}

func TestBackspaceOnSoftWrappedLine(t *testing.T) {
	t.Parallel()

	t.Run("backspace after newline on soft-wrapped text", func(t *testing.T) {
		t.Parallel()

		ta := input.NewArea()
		ta.SetWidth(20) // Small width to force wrapping
		ta.SetHeight(10)
		ta.Focus()

		e := &editor{
			textarea:  ta,
			userTyped: true,
		}

		// Enter text that overflows to line 2 (soft-wraps)
		longText := "this is a long text that wraps"
		e.textarea.SetValue(longText)
		e.textarea.MoveToEnd()

		t.Logf("After long text - Line: %d, LineInfo: %+v, Value: %q",
			e.textarea.Line(), e.textarea.LineInfo(), e.textarea.Value())

		// Press shift+enter to add a newline (simulated by directly adding \n)
		e.textarea.InsertString("\n")

		t.Logf("After newline - Line: %d, LineInfo: %+v, Value: %q",
			e.textarea.Line(), e.textarea.LineInfo(), e.textarea.Value())

		// Type a few characters
		e.textarea.InsertString("abc")

		t.Logf("After typing abc - Line: %d, LineInfo: %+v, Value: %q",
			e.textarea.Line(), e.textarea.LineInfo(), e.textarea.Value())

		// Now backspace
		_, _ = e.handleGraphemeBackspace()

		value := e.textarea.Value()
		t.Logf("After backspace - Line: %d, LineInfo: %+v, Value: %q",
			e.textarea.Line(), e.textarea.LineInfo(), value)

		// The value should be the long text + newline + "ab"
		expectedValue := longText + "\nab"
		assert.Equal(t, expectedValue, value, "value should have one char removed")

		// Cursor should still be on logical line 1 (the line after the newline)
		assert.Equal(t, 1, e.textarea.Line(), "cursor should stay on line 1")
	})
}
