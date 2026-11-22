package scrollview

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

type scrollbarClock struct{ now time.Time }

func (s *scrollbarClock) Now() time.Time { return s.now }
func (s *scrollbarClock) Tick(delay time.Duration, message func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg {
		s.now = s.now.Add(delay)
		return message(s.now)
	}
}

func animationFixture(t *testing.T) (*Model, *animation.Runtime, func(tea.Cmd) tea.Cmd) {
	t.Helper()
	m := New(WithReserveScrollbarSpace(true))
	m.SetSize(12, 4)
	m.SetPosition(10, 20)
	m.SetContent([]string{"0", "1", "2", "3", "4", "5", "6", "7"}, 8)
	clock := animation.NewRuntimeWithScheduler(&scrollbarClock{time.Unix(1, 0)})
	m.SetAnimationRuntime(clock)
	return m, clock, func(cmd tea.Cmd) tea.Cmd {
		t.Helper()
		require.NotNil(t, cmd)
		tick, ok := clock.Accept(cmd().(animation.TickMsg))
		require.True(t, ok)
		m.UpdateScrollbarAnimation(tick)
		return clock.Continue()
	}
}

func TestScrollbarPaintsFadeInOutAndReversal(t *testing.T) {
	m, _, step := animationFixture(t)
	hidden := m.View()
	require.NotContains(t, hidden, "│")
	x := m.ScrollbarX()
	handled, cmd := m.Update(tea.MouseMotionMsg{X: x - 1, Y: 20})
	require.False(t, handled, "hover does not consume a mouse event")
	require.Equal(t, hidden, m.View(), "starting the fade does not pop in")

	cmd = step(cmd)
	fadeIn := m.View()
	require.Contains(t, fadeIn, "│")
	require.Equal(t, ansi.Strip(hidden), strings.ReplaceAll(ansi.Strip(fadeIn), "│", " "))
	cmd = step(cmd)
	require.NotEqual(t, fadeIn, m.View(), "successive frames must paint distinct colors")
	cmd = step(cmd)
	full := m.View()
	require.Contains(t, full, "│")
	require.NotEqual(t, fadeIn, full)
	for cmd != nil {
		cmd = step(cmd)
	}
	require.Equal(t, full, m.View(), "hover remains fully shown after the hold")

	cmd = m.SetScrollbarPointer(-1, -1)
	require.Equal(t, full, m.View(), "starting the fade does not pop out")
	cmd = step(cmd)
	fadeOut := m.View()
	require.Contains(t, fadeOut, "│")
	require.NotEqual(t, full, fadeOut)
	require.Equal(t, ansi.Strip(full), ansi.Strip(fadeOut))

	cmd = step(cmd)
	nearlyHidden := m.View()
	require.Contains(t, nearlyHidden, "│")
	require.NotEqual(t, fadeOut, nearlyHidden)
	require.Nil(t, m.SetScrollbarPointer(x, 20), "reversal uses the same tick chain")
	require.Equal(t, nearlyHidden, m.View(), "reversal starts from the painted opacity")
	cmd = step(cmd)
	require.Equal(t, fadeOut, m.View(), "reversal paints back through the intermediate color")
	cmd = step(cmd)
	require.Equal(t, full, m.View())
	_ = m.SetScrollbarPointer(-1, -1)
	for cmd != nil {
		cmd = step(cmd)
	}
	require.Equal(t, hidden, m.View(), "idle restores the unpainted gutter without touching edge fades")
}

func TestScrollbarActivityAndDragOutliveHover(t *testing.T) {
	m, clock, step := animationFixture(t)
	_, cmd := m.Update(messages.WheelCoalescedMsg{Delta: -1})
	require.Nil(t, cmd, "a boundary wheel does not reveal")
	_, cmd = m.Update(messages.WheelCoalescedMsg{Delta: 1})
	cmd = step(cmd)
	partial := m.View()
	require.Contains(t, partial, "│")
	_, again := m.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	require.Nil(t, again, "a burst reuses the active tick chain")
	for clock.Now() < m.visibility.holdUntil {
		cmd = step(cmd)
	}
	require.Contains(t, m.View(), "│", "hold expiry starts the fade rather than erasing the bar")
	for cmd != nil {
		cmd = step(cmd)
	}
	require.NotContains(t, m.View(), "│")

	x := m.ScrollbarX()
	handled, cmd := m.Update(tea.MouseClickMsg{X: x, Y: 20, Button: tea.MouseLeft})
	require.True(t, handled)
	require.True(t, m.IsDragging())
	handled, again = m.Update(tea.MouseMotionMsg{X: x - 7, Y: 21, Button: tea.MouseLeft})
	require.True(t, handled)
	require.Nil(t, again)
	require.Positive(t, m.ScrollOffset())
	for cmd != nil {
		cmd = step(cmd)
	}
	require.Contains(t, m.View(), "│", "captured thumb remains visible outside the gutter and after the hold")
	handled, cmd = m.Update(tea.MouseReleaseMsg{X: x - 7, Y: 21, Button: tea.MouseLeft})
	require.True(t, handled)
	require.False(t, m.IsDragging())
	for cmd != nil {
		cmd = step(cmd)
	}
	require.NotContains(t, m.View(), "│")
}

func TestSettledHoverReconcilesGeometryAndContentChanges(t *testing.T) {
	settle := func(t *testing.T) (*Model, *animation.Runtime, string) {
		t.Helper()
		m, clock, step := animationFixture(t)
		cmd := m.SetScrollbarPointer(m.ScrollbarX(), 20)
		for cmd != nil {
			cmd = step(cmd)
		}
		require.False(t, clock.HasActive(), "a settled hover has no tick chain")
		full := m.View()
		require.Contains(t, full, "│")
		return m, clock, full
	}

	t.Run("pointer still over the bar", func(t *testing.T) {
		m, _, full := settle(t)
		m.SetSize(12, 4)
		m.SetPosition(10, 20)
		m.SetContent([]string{"0", "1", "2", "3", "4", "5", "6", "7"}, 8)
		require.Equal(t, full, m.View())
	})

	t.Run("move", func(t *testing.T) {
		m, clock, _ := settle(t)
		m.SetPosition(30, 20)
		require.NotContains(t, m.View(), "│", "a stationary pointer no longer over the moved bar")
		require.False(t, clock.HasActive())
	})

	t.Run("resize", func(t *testing.T) {
		m, clock, _ := settle(t)
		m.SetSize(20, 4)
		require.NotContains(t, m.View(), "│", "a stationary pointer no longer over the resized bar")
		require.False(t, clock.HasActive())
	})

	t.Run("content stops and resumes scrolling", func(t *testing.T) {
		m, clock, _ := settle(t)
		m.SetContent([]string{"0", "1"}, 2)
		require.Nil(t, m.SetScrollbarPointer(-1, -1), "a non-scrollable view starts no animation")
		m.SetContent([]string{"0", "1", "2", "3", "4", "5", "6", "7"}, 8)
		require.NotContains(t, m.View(), "│", "the old opacity is not restored when scrolling resumes")
		require.False(t, clock.HasActive())
	})
}

func TestLosingOverflowResetsInFlightFadeAndCapture(t *testing.T) {
	long := []string{"0", "1", "2", "3", "4", "5", "6", "7"}
	for _, tc := range []struct {
		name            string
		shrink, restore func(*Model)
	}{
		{"content fits", func(m *Model) { m.SetContent([]string{"0"}, 1) }, func(m *Model) { m.SetContent(long, len(long)) }},
		{"zero height", func(m *Model) { m.SetSize(12, 0) }, func(m *Model) { m.SetSize(12, 4) }},
	} {
		t.Run(tc.name+" during hover fade", func(t *testing.T) {
			m, clock, step := animationFixture(t)
			x := m.ScrollbarX()
			step(m.SetScrollbarPointer(x, 20))
			require.Contains(t, m.View(), "│", "one frame painted")
			require.Nil(t, m.SetScrollbarPointer(x-8, 20), "leaving reuses the active chain")

			tc.shrink(m)
			require.False(t, clock.HasActive(), "no scrollbar releases the tick lease at once")
			tc.restore(m)
			require.NotContains(t, m.View(), "│", "restored overflow does not resume the old fade")
			require.False(t, clock.HasActive())
			require.Equal(t, x-8, m.visibility.x, "the latest pointer is kept")
		})

		t.Run(tc.name+" during settled drag", func(t *testing.T) {
			m, clock, step := animationFixture(t)
			x := m.ScrollbarX()
			_, cmd := m.Update(tea.MouseClickMsg{X: x, Y: 20, Button: tea.MouseLeft})
			for cmd != nil {
				cmd = step(cmd)
			}
			require.True(t, m.IsDragging())
			require.False(t, clock.HasActive(), "a settled capture has no tick chain")

			tc.shrink(m)
			require.False(t, m.IsDragging(), "no scrollbar ends the capture")
			tc.restore(m)
			require.False(t, m.IsDragging())
			require.NotContains(t, m.View(), "│")
			handled, _ := m.Update(tea.MouseMotionMsg{X: x - 8, Y: 23, Button: tea.MouseLeft})
			require.False(t, handled, "motion is no longer captured")
			require.Zero(t, m.ScrollOffset())
		})
	}
}
