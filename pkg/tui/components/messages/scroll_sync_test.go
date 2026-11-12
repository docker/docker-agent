package messages

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestMouseReleasePreservesScrollBeforeView(t *testing.T) {
	t.Parallel()

	for _, initial := range []struct {
		name    string
		content string
	}{
		{"short", "start\n\n"},
		{"long", strings.Repeat("history paragraph\n\n", 40)},
	} {
		t.Run(initial.name, func(t *testing.T) {
			m := NewScrollableView(animation.NewRuntime(), 80, 12, &service.SessionState{}).(*model)
			t.Cleanup(m.ar.Stop)
			m.AppendToLastMessage("root", initial.content)
			m.View()

			// Blur defers View while streaming continues to advance the transcript.
			m.AppendToLastMessage("root", strings.Repeat("new paragraph\n\n", 40))
			offset := m.scrollOffset
			require.Positive(t, offset)
			require.False(t, m.userHasScrolled)

			_, _ = m.Update(tea.MouseReleaseMsg{X: 20, Y: 6, Button: tea.MouseLeft})
			require.Equal(t, offset, m.scrollOffset, "an unrelated release must not import stale scrollbar state")
			require.False(t, m.userHasScrolled, "an unrelated release must not disable follow-tail")
			m.View()
			require.Equal(t, offset, m.scrollOffset)
		})
	}
}

func TestScrollbarReleaseEndsInterruptedSelection(t *testing.T) {
	t.Parallel()

	m := NewScrollableView(animation.NewRuntime(), 80, 12, &service.SessionState{}).(*model)
	t.Cleanup(m.ar.Stop)
	m.AppendToLastMessage("root", strings.Repeat("history paragraph\n\n", 40))
	m.View()

	_, _ = m.Update(tea.MouseClickMsg{X: 10, Y: 6, Button: tea.MouseLeft})
	require.True(t, m.IsSelecting())

	// A release outside the terminal can leave selection active on refocus.
	x := m.scrollview.ScrollbarX()
	_, _ = m.Update(tea.MouseClickMsg{X: x, Y: m.height - 1, Button: tea.MouseLeft})
	require.True(t, m.IsScrollbarDragging())
	_, _ = m.Update(tea.MouseMotionMsg{X: x, Y: m.height - 3, Button: tea.MouseLeft})
	_, _ = m.Update(tea.MouseReleaseMsg{X: x, Y: m.height - 3, Button: tea.MouseLeft})
	require.False(t, m.IsScrollbarDragging())
	require.False(t, m.IsSelecting(), "release must also end the interrupted selection")

	offset := m.scrollOffset
	_, _ = m.Update(tea.MouseMotionMsg{X: 10, Y: 0})
	require.Equal(t, offset, m.scrollOffset, "button-free motion must not autoscroll")
}

func TestIgnoredScrollbarClickPreservesScrollBeforeView(t *testing.T) {
	t.Parallel()

	for _, button := range []tea.MouseButton{tea.MouseRight, tea.MouseMiddle} {
		t.Run(button.String(), func(t *testing.T) {
			m := NewScrollableView(animation.NewRuntime(), 80, 12, &service.SessionState{}).(*model)
			t.Cleanup(m.ar.Stop)
			m.AppendToLastMessage("root", "start\n\n")
			m.View()
			m.AppendToLastMessage("root", strings.Repeat("new paragraph\n\n", 40))
			offset := m.scrollOffset
			require.Positive(t, offset)

			x := m.scrollview.ScrollbarX()
			_, _ = m.Update(tea.MouseClickMsg{X: x, Y: 6, Button: button})
			_, _ = m.Update(tea.MouseReleaseMsg{X: x, Y: 6, Button: button})
			require.Equal(t, offset, m.scrollOffset)
			require.False(t, m.userHasScrolled)
			require.False(t, m.IsScrollbarDragging())
		})
	}
}

func TestScrollbarUsesCurrentGeometryBeforeView(t *testing.T) {
	t.Parallel()

	for _, grabBeforeStream := range []bool{false, true} {
		t.Run(map[bool]string{false: "click after stream", true: "release after stream"}[grabBeforeStream], func(t *testing.T) {
			m := NewScrollableView(animation.NewRuntime(), 80, 12, &service.SessionState{}).(*model)
			t.Cleanup(m.ar.Stop)
			m.AppendToLastMessage("root", strings.Repeat("history paragraph\n\n", 40))
			m.View()
			x, y := m.scrollview.ScrollbarX(), m.height-1
			if grabBeforeStream {
				_, _ = m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
				require.True(t, m.IsScrollbarDragging())
			}
			m.AppendToLastMessage("root", strings.Repeat("new paragraph\n\n", 40))
			offset := m.scrollOffset
			require.Positive(t, offset)
			if !grabBeforeStream {
				_, _ = m.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
				require.True(t, m.IsScrollbarDragging())
				require.Equal(t, offset, m.scrollOffset, "grabbing the thumb must not import stale geometry")
			}
			_, _ = m.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
			require.False(t, m.IsScrollbarDragging())
			require.Equal(t, offset, m.scrollOffset, "release without motion must preserve the current offset")
			require.False(t, m.userHasScrolled)
		})
	}
}

func TestScrollToBottomReconcilesDirtyGeometry(t *testing.T) {
	t.Parallel()

	for _, hover := range []bool{false, true} {
		t.Run(map[bool]string{false: "without hover", true: "with hover"}[hover], func(t *testing.T) {
			m := NewScrollableView(animation.NewRuntime(), 80, 12, &service.SessionState{}).(*model)
			t.Cleanup(m.ar.Stop)
			m.AppendToLastMessage("root", "START-OF-CONVERSATION\n\n"+strings.Repeat("history paragraph\n\n", 40))
			m.AddAssistantMessage("root", "")
			m.View()
			require.Positive(t, m.scrollOffset)

			// Replacing the spinner invalidates geometry while blurred View is deferred.
			m.AppendToLastMessage("root", "LATEST-RESPONSE\n\n")
			require.Zero(t, m.totalHeight)
			_, _ = m.Update(m.ScrollToBottom()())
			require.Positive(t, m.scrollOffset, "bottom-scroll must not clamp against invalidated height")
			if hover {
				_, _ = m.Update(tea.MouseMotionMsg{X: 20, Y: 6})
			}
			frame := m.View()
			require.Contains(t, frame, "LATEST-RESPONSE")
			require.NotContains(t, frame, "START-OF-CONVERSATION")
			require.False(t, m.userHasScrolled)
			require.Equal(t, m.totalScrollableHeight()-m.height, m.scrollOffset)

			m.AppendToLastMessage("root", strings.Repeat("new paragraph\n\n", 20)+"STILL-FOLLOWING\n\n")
			require.Contains(t, m.View(), "STILL-FOLLOWING")
		})
	}
}

func TestInteractionReconcilesDirtyGeometryWithoutScrollCommand(t *testing.T) {
	t.Parallel()

	for _, interaction := range []struct {
		name string
		run  func(*testing.T, *model)
	}{
		{"hover", func(_ *testing.T, m *model) {
			_, _ = m.Update(tea.MouseMotionMsg{X: 20, Y: 6})
		}},
		{"click", func(t *testing.T, m *model) {
			t.Helper()
			_, _ = m.Update(tea.MouseClickMsg{X: 20, Y: 6, Button: tea.MouseLeft})
			require.Equal(t, m.scrollOffset+6, m.selection.startLine, "hit-testing must use the reconciled offset")
		}},
		{"copy", func(t *testing.T, m *model) {
			t.Helper()
			m.selectRange(5, 0, 5, 30)
			_, cmd := m.Update(DebouncedCopyMsg{ClickID: m.selection.pendingCopyID})
			require.NotNil(t, cmd)
		}},
	} {
		t.Run(interaction.name, func(t *testing.T) {
			for _, scrolled := range []bool{false, true} {
				t.Run(map[bool]string{false: "following", true: "scrolled up"}[scrolled], func(t *testing.T) {
					m := NewScrollableView(animation.NewRuntime(), 80, 12, &service.SessionState{}).(*model)
					t.Cleanup(m.ar.Stop)
					m.AppendToLastMessage("root", strings.Repeat("history paragraph\n\n", 40))
					m.AddAssistantMessage("root", "")
					m.View()
					if scrolled {
						m.scrollPageUp()
					}
					offset := m.scrollOffset
					require.Positive(t, offset)

					cmd := m.AppendToLastMessage("root", strings.Repeat("new paragraph\n\n", 40)+"LATEST-RESPONSE\n\n")
					require.Nil(t, cmd, "this append has no bottom-scroll command to reconcile geometry")
					require.True(t, m.renderDirty)
					interaction.run(t, m)
					frame := m.View()
					require.Equal(t, scrolled, m.userHasScrolled)
					if scrolled {
						require.Equal(t, offset, m.scrollOffset)
						if interaction.name == "hover" {
							require.NotEmpty(t, m.deferredTail, "hover must not materialize the tail")
						}
						require.NotContains(t, frame, "LATEST-RESPONSE")
					} else {
						require.Contains(t, frame, "LATEST-RESPONSE")
						require.Equal(t, m.totalScrollableHeight()-m.height, m.scrollOffset)
						m.AppendToLastMessage("root", strings.Repeat("new paragraph\n\n", 20)+"STILL-FOLLOWING\n\n")
						require.Contains(t, m.View(), "STILL-FOLLOWING")
					}
				})
			}
		})
	}
}

func TestGeometryReconciliationPreservesKeyboardMessageSelection(t *testing.T) {
	t.Parallel()

	m := NewScrollableView(animation.NewRuntime(), 80, 12, &service.SessionState{}).(*model)
	t.Cleanup(m.ar.Stop)
	m.AppendToLastMessage("root", "EARLIER-MESSAGE\n\n"+strings.Repeat("history paragraph\n\n", 40))
	m.AddUserMessage("next question")
	m.AppendToLastMessage("root", "LATEST-RESPONSE\n\n")
	m.View()
	m.Focus()
	_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	require.Zero(t, m.selectedMessageIndex)
	require.False(t, m.userHasScrolled, "keyboard message selection does not set the manual scroll flag")
	offset := m.scrollOffset
	_, _ = m.Update(tea.MouseMotionMsg{X: 20, Y: 6})
	require.Contains(t, m.View(), "EARLIER-MESSAGE")
	require.Equal(t, offset, m.scrollOffset, "reconciliation must not unconditionally follow the bottom")
}
