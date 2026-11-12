package messages

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

type transcriptInvariant struct {
	total, offset, max       int
	userHasScrolled          bool
	activeStart, activeEnd   int
	deferredIndex, deferredN int
}

func captureTranscriptInvariant(m *model) transcriptInvariant {
	activeStart, activeEnd := -1, -1
	if m.activeSegments != nil {
		activeStart = m.activeSegments.start
		activeEnd = activeStart + m.activeSegments.height()
	}
	return transcriptInvariant{
		total: m.totalHeight, offset: m.scrollOffset, max: max(0, m.totalScrollableHeight()-m.height),
		userHasScrolled: m.userHasScrolled, activeStart: activeStart, activeEnd: activeEnd,
		deferredIndex: m.deferredTailIndex, deferredN: len(m.deferredTail),
	}
}

func assertTranscriptExistsAndIsCanonical(t *testing.T, m *model) {
	t.Helper()
	frame := m.View()
	if m.totalHeight > 0 && m.scrollOffset < m.totalHeight {
		require.NotEmpty(t, strings.TrimSpace(ansi.Strip(frame)), "nonempty in-range transcript produced an empty viewport")
	}
	if s := m.activeSegments; s != nil {
		require.Len(t, m.renderedLines, s.start, "active suffix must have exactly one owner; flattened prefix ends at its boundary")
		require.Equal(t, m.totalHeight, s.start+s.height(), "active suffix must end at total height")
	} else {
		require.Len(t, m.renderedLines, m.totalHeight, "flattened transcript must own every content line")
	}
}

func TestScrollHoverAlternationPreservesTranscriptRepresentation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*model)
	}{
		{"idle-flattened", func(m *model) {
			m.focused, m.selectedMessageIndex = true, len(m.messages)-1
			m.invalidateItem(len(m.messages) - 1)
			_ = m.View()
		}},
		{"active-virtual-suffix", func(m *model) {}},
		{"scrolled-deferred-tail", func(m *model) {
			m.scrollPageUp()
			for i := range 20 {
				m.AppendToLastMessage("root", fmt.Sprintf("deferred-%02d **bold** λ界\n\n", i))
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewScrollableView(animation.NewRuntime(), 72, 10, &service.SessionState{}).(*model)
			m.SetSize(72, 10)
			for i := range 3 {
				msg := types.Agent(types.MessageTypeAssistant, "root", fmt.Sprintf("history-%d\n\n%s", i, strings.Repeat("long transcript line\n\n", 60)))
				m.messages = append(m.messages, msg)
				m.views = append(m.views, m.createMessageView(msg))
			}
			_ = m.View()
			m.scrollToBottom()
			tc.setup(m)
			assertTranscriptExistsAndIsCanonical(t, m)

			// Deselecting makes the final assistant eligible for segmentation again.
			// The following real mouse transitions used to leave both a flattened and
			// virtual owner for the same suffix, allowing later cached frames to use
			// incompatible line geometry.
			m.focused, m.selectedMessageIndex = false, -1
			m.refreshRenderedItem(len(m.messages) - 1)
			assertTranscriptExistsAndIsCanonical(t, m)

			for _, delta := range []int{-1, -1_000_000, 1, 1_000_000, -3, 3, -3, 3} {
				m.scrollByWheel(delta)
				before := captureTranscriptInvariant(m)
				for _, motion := range []tea.MouseMotionMsg{
					{X: 20, Y: 1}, {X: 20, Y: m.height + 3}, {X: 20, Y: 1}, {X: 20, Y: m.height + 3},
				} {
					_, _ = m.handleMouseMotion(motion)
					assertTranscriptExistsAndIsCanonical(t, m)
					after := captureTranscriptInvariant(m)
					require.Equal(t, before, after, "hover/leave changed transcript geometry, follow state, boundaries, or materialization")
				}
			}
		})
	}
}
