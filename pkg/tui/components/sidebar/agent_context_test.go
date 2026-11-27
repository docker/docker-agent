package sidebar

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
)

// recordAgentUsage feeds a TokenUsageEvent through the sidebar's normal entry
// point, as the chat page does for live, sub-session, and background events.
func recordAgentUsage(m *model, sessionID, agentName string, contextLen, contextLimit int64) {
	m.SetTokenUsage(&runtime.TokenUsageEvent{
		SessionID:    sessionID,
		AgentContext: runtime.AgentContext{AgentName: agentName},
		Usage: &runtime.Usage{
			InputTokens:   contextLen / 2,
			OutputTokens:  contextLen - contextLen/2,
			ContextLength: contextLen,
			ContextLimit:  contextLimit,
		},
	})
}

// TestAgentRosterShowsPerAgentContextPercent verifies each card's metric line
// carries the agent's own labeled context percentage once that agent has
// emitted usage — and an explicit "—" for agents that have not run. At the
// default width this roster renders the compact vocabulary (root's preferred
// wide joined metric line would overflow), so the label reads "Ctx".
func TestAgentRosterShowsPerAgentContextPercent(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 40,
		runtime.AgentDetails{Name: "root", Provider: "anthropic", Model: "opus", Thinking: "high"},
		runtime.AgentDetails{Name: "developer", Provider: "openai", Model: "gpt-5.4", Thinking: "off"},
		runtime.AgentDetails{Name: "idle", Provider: "openai", Model: "gpt-4o", Thinking: "off"},
	)

	recordAgentUsage(m, "session-root", "root", 30_000, 100_000)
	recordAgentUsage(m, "session-child", "developer", 42_000, 100_000)

	assert.Contains(t, agentMetrics(m, "root"), "Ctx 30%",
		"root's card shows its own labeled context percent")
	_, rootLine2 := agentLines(m, "root")
	assert.Contains(t, rootLine2, "anthropic/opus", "model text stays on line 2")

	assert.Contains(t, agentMetrics(m, "developer"), "Ctx 42%",
		"developer's card shows its own labeled context percent")

	idleMetrics := agentMetrics(m, "idle")
	assert.Contains(t, idleMetrics, "Ctx —", "agents that never ran show an explicit —")
	assert.NotContains(t, idleMetrics, "%", "agents that never ran show no percent")
}

// TestAgentContextPercent_LatestSnapshotWins verifies a fresh delegation to the
// same agent (new sub-session) replaces its displayed context usage.
func TestAgentContextPercent_LatestSnapshotWins(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 40,
		runtime.AgentDetails{Name: "root", Provider: "anthropic", Model: "opus"},
	)

	recordAgentUsage(m, "session-1", "root", 80_000, 100_000)
	assert.Equal(t, "80%", m.agentContextPercent("root"))

	recordAgentUsage(m, "session-2", "root", 10_000, 100_000)
	assert.Equal(t, "10%", m.agentContextPercent("root"))
}

// TestAgentContextPercent_UnknownLimit verifies no percent is shown when the
// context limit is unknown (e.g. harness-backed agents) or nothing was
// recorded for the agent — the card shows an explicit "—" instead.
func TestAgentContextPercent_UnknownLimit(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 40,
		runtime.AgentDetails{Name: "root", Provider: "anthropic", Model: "opus"},
	)

	assert.Empty(t, m.agentContextPercent("root"), "no usage recorded yet")

	recordAgentUsage(m, "session-1", "root", 30_000, 0)
	assert.Empty(t, m.agentContextPercent("root"), "no percent without a context limit")

	metrics := agentMetrics(m, "root")
	assert.Contains(t, metrics, "Context —", "unknown context reads —, not a blank")
	assert.NotContains(t, metrics, "%")
}

// TestAgentContextPercent_BackgroundAgentUsage verifies a usage event from a
// detached background sub-session (forwarded out-of-band by the runtime, with
// its own session id) surfaces on the background agent's roster line without
// disturbing the parent's active-session accounting.
func TestAgentContextPercent_BackgroundAgentUsage(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, 40,
		runtime.AgentDetails{Name: "root", Provider: "anthropic", Model: "opus"},
		runtime.AgentDetails{Name: "worker", Provider: "openai", Model: "gpt-5.4"},
	)

	// Parent runs on the main session; the background worker's events carry
	// its own sub-session id and interleave with the parent's.
	m.rootSessionID = "session-root"
	recordAgentUsage(m, "session-root", "root", 20_000, 100_000)
	recordAgentUsage(m, "bg-session", "worker", 55_000, 100_000)

	assert.Contains(t, agentMetrics(m, "worker"), "Context 55%",
		"background worker's card shows its own context percent")

	assert.Equal(t, "20%", m.contextPercent(),
		"the main context gauge keeps tracking the active session, not the background one")
}

// TestAgentCardMetricLinesFitNarrowWidth verifies the labeled metrics wrap
// into lines that never exceed the content width at the minimum sidebar
// width, instead of colliding with or truncating one another.
func TestAgentCardMetricLinesFitNarrowWidth(t *testing.T) {
	t.Parallel()

	m := newAgentPanelSidebar(t, MinWidth,
		runtime.AgentDetails{Name: "root", Provider: "anthropic", Model: "claude-sonnet-4-6", Thinking: "high"},
	)
	recordAgentUsage(m, "session-1", "root", 100_000, 100_000)

	contentWidth := m.contentWidth(false)
	card := agentCard(m, "root")
	require.NotEmpty(t, card)
	for _, line := range card {
		assert.LessOrEqualf(t, lipgloss.Width(line), contentWidth,
			"card line must fit the content width: %q", line)
	}

	metrics := strings.Join(card[2:], "\n")
	assert.Contains(t, metrics, "Eff high", "the compact effort label keeps its semantic value")
	assert.NotContains(t, metrics, gaugePattern(4), "the decorative gauge is omitted at the minimum width")
	assert.Contains(t, metrics, "Ctx 100%", "the compact context label keeps its percent")
}

func TestSetAgentInfoUpdatesPerAgentContextLimit(t *testing.T) {
	t.Parallel()
	m := newTestSidebar(t)
	m.startStream("session-1", "root")
	recordAgentUsage(m.model, "session-1", "root", 30, 100)
	assert.Equal(t, "30%", m.agentContextPercent("root"))

	m.SetAgentInfo("root", "openai/gpt-5", "", 200, "", 0)
	assert.Equal(t, "15%", m.contextPercent())
	assert.Equal(t, "15%", m.agentContextPercent("root"))
}

func TestSetTokenUsageAccountingUpdatePreservesAgentContext(t *testing.T) {
	t.Parallel()
	m := newTestSidebar(t)
	m.startStream("session-1", "root")
	recordAgentUsage(m.model, "session-1", "root", 30, 100)
	assert.Equal(t, "30%", m.agentContextPercent("root"))

	m.SetTokenUsage(&runtime.TokenUsageEvent{
		SessionID:    "session-1",
		AgentContext: runtime.AgentContext{AgentName: "root"},
		Usage:        &runtime.Usage{Cost: 0.42},
	})
	assert.Equal(t, "30%", m.contextPercent())
	assert.Equal(t, "30%", m.agentContextPercent("root"))
}

func TestSetAgentInfoUpdatesContextLimitAfterSameSessionHandoff(t *testing.T) {
	t.Parallel()
	m := newTestSidebar(t)
	m.startStream("session-1", "root")
	recordAgentUsage(m.model, "session-1", "root", 30, 100)

	// Same-session handoff to helper updates workingAgent while sessionStack keeps root.
	m.Update(&runtime.AgentInfoEvent{
		SessionID:    "session-1",
		AgentName:    "helper",
		Model:        "openai/gpt-5",
		ContextLimit: 100,
	})
	recordAgentUsage(m.model, "session-1", "helper", 30, 100)
	assert.Equal(t, "30%", m.contextPercent())
	assert.Equal(t, "30%", m.agentContextPercent("helper"))

	// Subsequent context-limit update for helper must apply to both session and agent.
	m.Update(&runtime.AgentInfoEvent{
		SessionID:    "session-1",
		AgentName:    "helper",
		Model:        "openai/gpt-5",
		ContextLimit: 200,
	})
	assert.Equal(t, "15%", m.contextPercent())
	assert.Equal(t, "15%", m.agentContextPercent("helper"))
}
