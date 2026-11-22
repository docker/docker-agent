package messages

import (
	"testing"
	"testing/synctest"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/core/input"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestInlineEditBlinkPreservesTranscript(t *testing.T) {
	for _, history := range []struct {
		name string
		size int
	}{{"short", 4}, {"long", 1000}} {
		t.Run(history.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m := NewScrollableView(animation.NewRuntime(), 72, 12, &service.SessionState{}).(*model)
				for range history.size {
					m.AddUserMessage("transcript history")
				}
				index := history.size / 2
				m.StartInlineEdit(index, index, "multiline edit\nwith a second line")
				cursor := m.CursorInput()
				cmd := input.Expose(nil, cursor)
				m.updateScrollState()
				m.userHasScrolled = true
				m.setScrollOffset(m.lineOffsets[index])
				visible := m.View()
				require.Contains(t, ansi.Strip(visible), "with a second line")
				lines, offsets := &m.renderedLines[0], &m.lineOffsets[0]
				geometry := captureTranscriptInvariant(m)
				generation := m.VisualGeneration()

				tick := cmd().(*input.BlinkMsg)
				next := tick.Apply(cursor)
				require.NotNil(t, next)
				hidden := m.View()
				require.NotEqual(t, visible, hidden, "the caret must visibly blink")
				require.Equal(t, ansi.Strip(visible), ansi.Strip(hidden), "blinking must not change text")
				require.Greater(t, m.VisualGeneration(), generation)
				require.Equal(t, geometry, captureTranscriptInvariant(m))
				require.Same(t, offsets, &m.lineOffsets[0], "blinking rebuilt all transcript offsets (history: %d items)", history.size)
				require.Same(t, lines, &m.renderedLines[0], "blinking rebuilt all transcript lines (history: %d items)", history.size)

				// Real edits still update text and layout, and wake the caret.
				m.Update(tea.PasteMsg{Content: "\na pasted line"})
				require.True(t, cursor.VirtualCursor())
				require.Contains(t, ansi.Strip(m.View()), "a pasted line")
				require.Equal(t, geometry.total+1, m.totalHeight)

				m.CancelInlineEdit()
				input.Expose(cursor, nil)
				restored := ansi.Strip(m.View())
				require.Contains(t, restored, "transcript history")
				require.NotContains(t, restored, "a pasted line")
			})
		})
	}
}
