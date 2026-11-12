package sidebar

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestBackgroundAgentsCount(t *testing.T) {
	t.Parallel()

	for _, mode := range []AgentInfoMode{AgentInfoCompact, AgentInfoDetailed} {
		t.Run(map[AgentInfoMode]string{AgentInfoCompact: "compact", AgentInfoDetailed: "detailed"}[mode], func(t *testing.T) {
			t.Parallel()
			m := newCompactPanelSidebar(t, 40, activeAgentsRoster()...)
			m.SetAgentInfoMode(mode)
			m.SetActiveAgentsOnly(true)
			m.Update(runtime.StreamStarted("root-session", "root"))

			assert.NotContains(t, m.View(), "Background:")
			assert.NotContains(t, m.collapsedInfoLine(120), "Background:")
			m.Update(runtime.BackgroundAgentStatus("child-1", "reviewer", true))
			m.Update(runtime.BackgroundAgentStatus("child-1", "reviewer", true))
			assert.Contains(t, ansi.Strip(m.View()), "Background: 1 running")
			assert.Contains(t, ansi.Strip(m.collapsedInfoLine(120)), "Background: 1 running")
			assert.Equal(t, []string{"root", "reviewer"}, rosterOwners(m))
			assert.Equal(t, "root", m.workingAgent)
			assert.Equal(t, "root", m.sessionState.CurrentAgentName())

			m.Update(runtime.BackgroundAgentStatus("child-2", "reviewer", true))
			m.Update(runtime.BackgroundAgentStatus("", "reviewer", true))
			m.Update(runtime.StreamStopped("root-session", "root", "normal"))
			m.Update(messages.StreamCancelledMsg{})
			assert.Contains(t, ansi.Strip(m.View()), "Background: 2 running")
			assert.Contains(t, ansi.Strip(m.collapsedInfoLine(120)), "Background: 2 running")
			assert.Empty(t, m.workingAgent)
			assert.Empty(t, m.sessionStack)

			m.Update(runtime.BackgroundAgentStatus("unknown", "reviewer", false))
			m.Update(runtime.BackgroundAgentStatus("child-1", "reviewer", false))
			m.Update(runtime.BackgroundAgentStatus("child-1", "reviewer", false))
			assert.Contains(t, ansi.Strip(m.View()), "Background: 1 running")
			m.Update(runtime.BackgroundAgentStatus("child-2", "reviewer", false))
			assert.NotContains(t, m.View(), "Background:")
			assert.NotContains(t, m.collapsedInfoLine(120), "Background:")
			assert.Equal(t, []string{"root"}, rosterOwners(m))
		})
	}
}

func TestBackgroundAgentsVisibilityAndClickOwners(t *testing.T) {
	t.Parallel()

	m := newCompactPanelSidebar(t, 40, activeAgentsRoster()...)
	m.Update(runtime.BackgroundAgentStatus("child", "reviewer", true))
	for i, line := range agentBody(m) {
		if strings.Contains(line, "Background:") {
			assert.Empty(t, m.agentLineOwners[i], "the running count must not switch agents on click")
		}
	}
	m.SetSectionVisibility(SectionVisibility{HideAgents: true})
	assert.NotContains(t, m.View(), "Background:")
	assert.NotContains(t, m.collapsedInfoLine(120), "Background:")
}

func TestBackgroundAgentsSessionReload(t *testing.T) {
	t.Parallel()

	m := newCompactPanelSidebar(t, 40, activeAgentsRoster()...)
	sess := session.New()
	m.LoadFromSession(sess)
	m.Update(runtime.BackgroundAgentStatus("child", "reviewer", true))
	m.LoadFromSession(sess)
	require.Len(t, m.backgroundAgents, 1, "reloading the same session preserves live tasks")
	m.LoadFromSession(session.New())
	assert.Empty(t, m.backgroundAgents, "a different conversation must not inherit tasks")
}

func TestBackgroundAgentsWrappedCountClickTargets(t *testing.T) {
	t.Parallel()

	m := newCompactPanelSidebar(t, MinWidth, activeAgentsRoster()...)
	m.SetSectionVisibility(SectionVisibility{HideUsage: true, HideTools: true, HideTodos: true})
	m.SetSize(MinWidth, 8)
	m.Update(runtime.BackgroundAgentStatus("child", "reviewer", true))
	m.View()
	require.True(t, m.cachedNeedsScrollbar)
	m.scrollview.ScrollToBottom()
	m.View()

	var summaryLines []int
	for i, line := range m.cachedLines {
		if strings.Contains(ansi.Strip(line), "Background:") {
			summaryLines = append(summaryLines, i, i+1)
		}
	}
	require.Len(t, summaryLines, 2, "the count wraps at minimum width with a scrollbar")
	for _, row := range summaryLines {
		y := row - m.scrollview.ScrollOffset()
		require.GreaterOrEqual(t, y, 0)
		require.Less(t, y, m.height)
		kind, name := m.HandleClickType(m.layoutCfg.PaddingLeft+1, y)
		assert.Equal(t, ClickNone, kind)
		assert.Empty(t, name)
	}
}
