package messages

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// freshWorkView renders the exchange built from scratch with the given answer.
func freshWorkView(t *testing.T, answer string, width int) string {
	t.Helper()
	m, _ := workFixtureWith(t, answer)
	m.SetCompactWorkLog(true)
	m.SetSize(width, 40)
	return ansi.Strip(m.View())
}

// assistantIndex returns the source index of the final answer row.
func assistantIndex(m *model) int { return len(m.messages) - 1 }

func TestCompactWorkContentAppendMatchesFreshRender(t *testing.T) {
	m, _ := workFixtureWith(t, "final answer")
	m.SetCompactWorkLog(true)
	_ = m.View()
	answer := m.messages[assistantIndex(m)]
	answer.Content += " and more"
	m.invalidateItem(assistantIndex(m))
	require.Equal(t, freshWorkView(t, "final answer and more", 80), ansi.Strip(m.View()), "an appended answer re-renders exactly as a fresh transcript")
}

func TestCompactWorkToolOutputChangeMatchesFreshRender(t *testing.T) {
	m, _ := workFixtureWith(t, "final answer")
	m.SetCompactWorkLog(true)
	_ = m.View()
	tool := m.messages[2]
	require.Equal(t, types.MessageTypeToolCall, tool.Type)
	tool.ToolStatus = types.ToolStatusError
	m.invalidateItem(2)
	m.SetWorkStatus(false, 0)

	fresh, _ := workFixtureWith(t, "final answer")
	freshTool := fresh.messages[2]
	freshTool.ToolStatus = types.ToolStatusError
	fresh.invalidateItem(2)
	fresh.SetCompactWorkLog(true)
	fresh.SetWorkStatus(false, 0)
	require.Equal(t, ansi.Strip(fresh.View()), ansi.Strip(m.View()), "a tool status change re-renders its Work row and header")
}

func TestCompactWorkResizeMatchesFreshRender(t *testing.T) {
	m, _ := workFixtureWith(t, "final answer")
	m.SetCompactWorkLog(true)
	_ = m.View()
	m.SetSize(50, 40)
	require.Equal(t, freshWorkView(t, "final answer", 50), ansi.Strip(m.View()), "a narrower transcript re-renders exactly as a fresh one")
}

func TestCompactWorkThemeChangeMatchesFreshRender(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	m, _ := workFixtureWith(t, "final answer")
	m.SetCompactWorkLog(true)
	_ = m.View()
	theme := styles.DefaultTheme()
	theme.Colors.Accent = "#e719a5"
	styles.ApplyTheme(theme)
	// The app announces a theme switch to the transcript, which drops its caches.
	m.Update(messages.ThemeChangedMsg{})
	fresh, _ := workFixtureWith(t, "final answer")
	fresh.SetCompactWorkLog(true)
	require.Equal(t, ansi.Strip(fresh.View()), ansi.Strip(m.View()), "a theme change re-renders the Work header and rows")
	require.Equal(t, fresh.View(), m.View(), "and the styled output matches a fresh transcript under the same theme")
}

func TestCompactWorkHoverAddsAndRemovesOnlyTheHoverState(t *testing.T) {
	m, _ := workFixtureWith(t, "final answer")
	m.SetCompactWorkLog(true)
	expandWork(t, m)
	before := m.View()
	childLine := thinkingLine(t, m)
	m.handleMouseMotion(tea.MouseMotionMsg{X: 4, Y: childLine - m.scrollOffset})
	m.View()
	m.handleMouseMotion(tea.MouseMotionMsg{X: 79, Y: m.yPos + m.height - 1})
	require.Equal(t, before, m.View(), "leaving the grouped row restores the transcript exactly")
}

func TestCompactWorkSpinnerTicksLeaveTheCachedAnswerIntact(t *testing.T) {
	m, ar := workFixtureWith(t, "final answer")
	m.SetCompactWorkLog(true)
	_ = m.View()
	answerRow := assistantIndex(m)
	_, cached := m.renderedItems.Get(answerRow)
	cmd := m.SetWorkStatus(true, 0)
	first := ansi.Strip(m.View())
	for range 30 {
		require.NotNil(t, cmd)
		tick, ok := ar.Accept(cmd().(animation.TickMsg))
		require.True(t, ok)
		m.Update(tick)
		cmd = ar.Continue()
	}
	later := ansi.Strip(m.View())
	require.NotEqual(t, first, later, "the spinner frame moves on accepted ticks")
	require.Contains(t, later, "final answer", "the answer row is still rendered after ticks")
	_, stillCached := m.renderedItems.Get(answerRow)
	require.Equal(t, cached, stillCached, "ticks keep unrelated cached rows")
}

func TestCompactWorkSteadyFramesAndHoverDoNotRebuildTheProjection(t *testing.T) {
	m, _ := workFixtureWith(t, "final answer")
	m.SetCompactWorkLog(true)
	_ = m.View()
	rebuilds := m.work.rebuilds
	expandWork(t, m)
	for i := range 20 {
		m.scrollOffset = i % max(1, m.totalHeight-m.height)
		m.invalidateView()
		_ = m.View()
	}
	m.handleMouseMotion(tea.MouseMotionMsg{X: 4, Y: thinkingLine(t, m) - m.scrollOffset})
	_ = m.View()
	require.Equal(t, rebuilds, m.work.rebuilds, "steady frames, scrolls and hover reuse the cached projection")

	m.messages[assistantIndex(m)].Content = ""
	m.invalidateItem(assistantIndex(m))
	_ = m.View()
	require.Greater(t, m.work.rebuilds, rebuilds, "a prose change re-projects the exchange")
}

func TestCompactWorkVisibleRepliesPolicyChangesTheFold(t *testing.T) {
	m, _ := workFixtureWith(t, "final answer")
	m.SetCompactWorkLog(true)
	require.Equal(t, messages.VisibleRepliesThree, m.VisibleReplies(), "the default policy keeps three replies")
	m.SetVisibleReplies(messages.VisibleReplies("latest"))
	require.Equal(t, messages.VisibleRepliesLatest, m.VisibleReplies())
	m.SetVisibleReplies(messages.VisibleReplies("future"))
	require.Equal(t, messages.VisibleRepliesThree, m.VisibleReplies(), "unknown policies fall back to three")
	require.NotEmpty(t, strings.TrimSpace(ansi.Strip(m.View())))
}

func TestCompactWorkAgentColorChangeMatchesFreshRender(t *testing.T) {
	t.Cleanup(func() { styles.SetAgentOrder(nil) })
	styles.SetAgentOrder([]string{"root"})
	m, _ := workFixtureWith(t, "final answer")
	m.SetCompactWorkLog(true)
	_ = m.View()
	styles.SetAgentOrder([]string{"other", "root"})
	fresh, _ := workFixtureWith(t, "final answer")
	fresh.SetCompactWorkLog(true)
	require.Equal(t, fresh.View(), m.View(), "an agent colour change re-renders badges and the Work header like a fresh transcript")
}
