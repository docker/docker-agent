package messages

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestEarlierDeferredTailKeepsFinalAnswerGeometry(t *testing.T) {
	t.Parallel()

	for _, transition := range []struct {
		name string
		run  func(*model)
	}{
		{"reasoning", func(m *model) { m.AppendReasoning("root", "Thinking") }},
		{"message group", func(m *model) { m.BreakMessageGroup() }},
	} {
		t.Run(transition.name, func(t *testing.T) {
			t.Parallel()

			for _, finish := range []struct {
				name string
				run  func(*model)
			}{
				{"stop", func(m *model) { m.FinalizeStream() }},
				{"cancel", func(m *model) { _, _ = m.Update(msgtypes.StreamCancelledMsg{}) }},
			} {
				t.Run(finish.name, func(t *testing.T) {
					t.Parallel()

					m := NewScrollableView(animation.NewRuntime(), 60, 8, &service.SessionState{}).(*model)
					t.Cleanup(m.ar.Stop)
					commentary := strings.Repeat("Earlier paragraph\n\n", 12)
					buffered := strings.Repeat("Buffered paragraph\n\n", 8)
					answer := strings.Repeat("Final answer paragraph\n\n", 10) + "FINAL-ANSWER-END"
					m.AppendToLastMessage("root", commentary)
					m.View()
					m.scrollToTop()
					m.AppendToLastMessage("root", buffered)
					transition.run(m)
					m.AppendToLastMessage("root", answer)
					m.View()
					require.NotEmpty(t, m.deferredTail)
					require.NotNil(t, m.activeSegments)
					start := m.activeSegments.start

					finish.run(m)
					require.Empty(t, m.deferredTail)
					require.Equal(t, commentary+buffered, m.messages[0].Content)
					require.Equal(t, answer, m.messages[len(m.messages)-1].Content)
					require.True(t, m.userHasScrolled)
					require.Zero(t, m.scrollOffset, "finishing must preserve the scrolled-up viewport")
					require.Greater(t, m.activeSegments.start, start)
					require.Equal(t, len(m.renderedLines), m.activeSegments.start)
					require.Equal(t, m.lineOffsets[len(m.views)-1], m.activeSegments.start)
					require.Equal(t, m.totalHeight, m.activeSegments.start+m.activeSegments.height())

					// End refreshes the final item, masking the stale offset; wheel scrolling does not.
					m.scrollByWheel(m.totalHeight)
					frame := m.View()
					require.Contains(t, ansi.Strip(frame), "FINAL-ANSWER-END")
					m.invalidateAllItems()
					require.Equal(t, frame, m.View(), "incremental geometry must match a full rebuild")
				})
			}
		})
	}
}
