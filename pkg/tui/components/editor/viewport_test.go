package editor

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestResizeKeepsWrappedDraftTailInView(t *testing.T) {
	t.Parallel()

	e := New(nil)
	e.SetSize(160, 3)
	draft := "first " + strings.Repeat("aaaa bbbb cccc dddd ", 6) + "END"
	e.Update(tea.PasteMsg{Content: draft})

	// A narrow terminal must reflow the draft before positioning its cursor.
	// No additional keypress or paint should be needed after resizing.
	e.SetSize(6, 3)
	width, height := e.GetSize()
	view := e.View()
	require.Equal(t, draft, e.Value())
	require.Contains(t, ansi.Strip(view), "END")
	require.NotContains(t, ansi.Strip(view), "first", "draft reflows and scrolls at the allocated width")
	require.LessOrEqual(t, lipgloss.Width(view), width)
	require.Equal(t, height, lipgloss.Height(view), "the frame is included in the allocation")

	e.SetSize(160, 3)
	require.Contains(t, ansi.Strip(e.View()), draft)

	// Resizing must not move the edit position or lose any of the value.
	e.Update(tea.KeyPressMsg{Code: '!', Text: "!"})
	require.Equal(t, draft+"!", e.Value())
	require.Contains(t, ansi.Strip(e.View()), draft+"!")
}

func TestContentLineCountUsesBubblesWrapWithoutMovingEditor(t *testing.T) {
	t.Parallel()

	e := New(nil).(*editor)
	e.SetSize(12, 3)
	e.SetValue("abcd\n界界\na\n")
	before := e.View()
	cursorRow, cursorColumn, offset := e.textarea.Line(), e.textarea.Column(), e.textarea.ScrollYOffset()
	require.Equal(t, 6, e.ContentLineCount(4), "exactly-full rows leave a continuation row for insertion")
	require.Equal(t, 4, e.ContentLineCount(12), "include trailing explicit newline")
	require.Equal(t, before, e.View())
	require.Equal(t, cursorRow, e.textarea.Line())
	require.Equal(t, cursorColumn, e.textarea.Column())
	require.Equal(t, offset, e.textarea.ScrollYOffset())
	e.SetValue("")
	require.Equal(t, 1, e.ContentLineCount(1), "a wrapped placeholder is not draft content")
}

func TestContentLineCountTracksNativeWrapAndEditorWidth(t *testing.T) {
	t.Parallel()

	for _, width := range []int{1, 2, 10, 24, 520} {
		for _, value := range []string{"", "abcdefghij", "a  b    c", "界界界", "e\u0301e\u0301e\u0301", "one\ntwo long lines  here\n", strings.Repeat("w", 500)} {
			e := New(nil).(*editor)
			frameWidth := styles.EditorStyle.GetHorizontalFrameSize()
			e.SetSize(width-frameWidth, 3)
			e.SetValue(value)
			probe := New(nil).(*editor)
			probe.SetSize(width-frameWidth, 1)
			want := 0
			for line := range strings.SplitSeq(value, "\n") {
				probe.textarea.SetValue(line)
				want += probe.textarea.LineInfo().Height
			}
			require.Equal(t, want, e.ContentLineCount(width-frameWidth), "width=%d draft=%q", width, value)
			require.Equal(t, value, e.Value())
			allocated, _ := e.GetSize()
			require.Equal(t, width, allocated)
			require.Equal(t, allocated, lipgloss.Width(e.View()))
		}
	}
}
