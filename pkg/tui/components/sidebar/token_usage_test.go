package sidebar

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// TestTokenUsageLine_ShowsCompactionCap verifies the usage line marks a
// capped effective limit with a short warning marker, and stays silent once
// SetAgentInfo reports an uncapped/absent compaction model.
func TestTokenUsageLine_ShowsCompactionCap(t *testing.T) {
	t.Parallel()

	m := newTestSidebar(t)
	m.SetAgentInfo("root", "anthropic/claude", "", 16_000, "local/small", 128_000)

	line := ansi.Strip(m.tokenUsageLine())
	assert.Contains(t, line, "⚠ capped")

	m.SetAgentInfo("root", "anthropic/claude", "", 128_000, "", 0)
	line = ansi.Strip(m.tokenUsageLine())
	assert.NotContains(t, line, "capped")
}

func TestActiveSessionTokens_SingleSession(t *testing.T) {
	t.Parallel()

	m := newTestSidebar(t)
	m.startStream("session-1", "root")
	m.recordUsageTokens("session-1", "root", 5000, 3000)

	tokens, found := m.activeSessionTokens()
	assert.True(t, found)
	assert.Equal(t, int64(8000), tokens)
}

// TestActiveSessionTokens_TracksActiveSubSession verifies the panel shows the
// running sub-session's tokens while it is active, then the parent's again once
// the sub-session stops.
func TestActiveSessionTokens_TracksActiveSubSession(t *testing.T) {
	t.Parallel()

	m := newTestSidebar(t)

	m.startStream("session-root", "root")
	m.recordUsageTokens("session-root", "root", 20000, 10000)

	// Sub-agent runs as a nested stream.
	m.startStream("session-child", "developer")
	m.recordUsageTokens("session-child", "developer", 8000, 2000)

	tokens, found := m.activeSessionTokens()
	assert.True(t, found)
	assert.Equal(t, int64(10000), tokens, "while the sub-agent runs, show its tokens")

	// Sub-agent done — back to the parent's tokens.
	m.stopStream()
	tokens, found = m.activeSessionTokens()
	assert.True(t, found)
	assert.Equal(t, int64(30000), tokens, "after the sub-agent returns, show the parent's tokens")
}

func TestActiveSessionTokens_FallbackToSingleSession(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	m := New(animation.NewRuntime(), t.Context(), sessionState).(*model)

	m.sessionUsage["session-1"] = &sessionUsage{Usage: runtime.Usage{
		InputTokens:  5000,
		OutputTokens: 5000,
	}}

	tokens, found := m.activeSessionTokens()
	assert.True(t, found)
	assert.Equal(t, int64(10000), tokens)
}

func TestActiveSessionTokens_Empty(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	m := New(animation.NewRuntime(), t.Context(), sessionState).(*model)

	tokens, found := m.activeSessionTokens()
	assert.False(t, found)
	assert.Equal(t, int64(0), tokens)
}

// TestActiveSessionTokens_StableDuringSubAgent verifies the reported count does
// not flicker while a sub-agent is the active session: it consistently reports
// the sub-session's tokens regardless of which agent last emitted an event.
func TestActiveSessionTokens_StableDuringSubAgent(t *testing.T) {
	t.Parallel()

	m := newTestSidebar(t)

	m.startStream("session-root", "root")
	m.recordUsageTokens("session-root", "root", 20000, 10000)

	m.startStream("session-child", "developer")
	m.recordUsageTokens("session-child", "developer", 8000, 2000)

	// Even if the parent agent name lingers in currentAgent, the active
	// session is the sub-session and must be reported consistently.
	m.currentAgent = "root"
	for range 100 {
		tokens, found := m.activeSessionTokens()
		assert.True(t, found)
		assert.Equal(t, int64(10000), tokens, "activeSessionTokens() flickered while a sub-agent was running")
	}
}

// TestActiveSessionTokens_RecoversFromImbalancedStreams verifies that a new
// top-level run starting with a reset is not pinned to a leaked sub-session
// from a previous run whose stream events were left unbalanced.
func TestActiveSessionTokens_RecoversFromImbalancedStreams(t *testing.T) {
	t.Parallel()

	m := newTestSidebar(t)

	// Turn 1: a sub-agent stream is left unbalanced (no matching stop).
	m.startStream("session-root", "root")
	m.recordUsageTokens("session-root", "root", 20000, 10000) // 30000
	m.startStream("session-child", "developer")
	m.recordUsageTokens("session-child", "developer", 8000, 2000) // 10000

	// Turn 2: a new top-level run resets tracking, runs, and completes.
	m.ResetStreamTracking()
	m.startStream("session-root", "root")
	m.recordUsageTokens("session-root", "root", 25000, 10000) // 35000
	m.stopStream()

	// Idle after the turn: must show the main session, not the leaked child.
	tokens, found := m.activeSessionTokens()
	assert.True(t, found)
	assert.Equal(t, int64(35000), tokens)
}

func TestTokenUsageSummary_SingleSession(t *testing.T) {
	t.Parallel()

	m := newTestSidebar(t)
	m.startStream("session-1", "root")
	m.SetTokenUsage(&runtime.TokenUsageEvent{
		SessionID:    "session-1",
		AgentContext: runtime.AgentContext{AgentName: "root"},
		Usage: &runtime.Usage{
			InputTokens:   5000,
			OutputTokens:  3000,
			ContextLength: 8000,
			ContextLimit:  100000,
			Cost:          0.05,
		},
	})

	summary := ansi.Strip(m.tokenUsageSummary())
	// Single session: shows total tokens, context, and cost in the same
	// compact format as the vertical Token Usage tab
	assert.Contains(t, summary, "8.0K")
	assert.Contains(t, summary, "$0.05")
	assert.Contains(t, summary, "(8%)")
	assert.NotContains(t, summary, "sub-sessions")
}

func TestTokenUsageSummary_MultipleSessions_ShowsActiveSessionTokens(t *testing.T) {
	t.Parallel()

	m := newTestSidebar(t)

	// Root agent session: 30K tokens, $0.10
	m.startStream("session-root", "root")
	m.SetTokenUsage(&runtime.TokenUsageEvent{
		SessionID:    "session-root",
		AgentContext: runtime.AgentContext{AgentName: "root"},
		Usage: &runtime.Usage{
			InputTokens:   20000,
			OutputTokens:  10000,
			ContextLength: 30000,
			ContextLimit:  100000,
			Cost:          0.10,
		},
	})

	// Child agent session: 10K tokens, $0.05
	m.startStream("session-child", "developer")
	m.SetTokenUsage(&runtime.TokenUsageEvent{
		SessionID:    "session-child",
		AgentContext: runtime.AgentContext{AgentName: "developer"},
		Usage: &runtime.Usage{
			InputTokens:   8000,
			OutputTokens:  2000,
			ContextLength: 10000,
			ContextLimit:  200000,
			Cost:          0.05,
		},
	})

	// While the sub-agent runs, show its tokens and context, with the
	// aggregated cost and sub-session count.
	summary := ansi.Strip(m.tokenUsageSummary())
	assert.Contains(t, summary, "10.0K")
	assert.Contains(t, summary, "$0.15")
	assert.Contains(t, summary, "(5%)")
	assert.Contains(t, summary, "(1 sub-sessions)")

	// Once it returns, the parent's tokens and context are shown again.
	m.stopStream()
	summary = ansi.Strip(m.tokenUsageSummary())
	assert.Contains(t, summary, "30.0K")
	assert.Contains(t, summary, "$0.15")
	assert.Contains(t, summary, "(30%)")
}

// TestTokenUsageSummary_NoUsageYet_ShowsZeroLine verifies the collapsed band
// carries the usage line from startup (before any message is sent), matching
// the vertical Token Usage tab (#top-band startup parity).
func TestTokenUsageSummary_NoUsageYet_ShowsZeroLine(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	m := New(animation.NewRuntime(), t.Context(), sessionState).(*model)

	summary := ansi.Strip(m.tokenUsageSummary())
	assert.Contains(t, summary, styles.TokenGlyph+" 0")
	assert.Contains(t, summary, "$0.00")
}

// TestTokenUsageTab_ShowsTokenGlyph verifies the vertical Token Usage tab line
// is prefixed with the shared token glyph (◉).
func TestTokenUsageTab_ShowsTokenGlyph(t *testing.T) {
	t.Parallel()

	m := newTestSidebar(t)
	m.startStream("session-1", "root")
	m.recordUsageTokens("session-1", "root", 5000, 3000)

	out := ansi.Strip(m.tokenUsage(40))
	assert.Contains(t, out, styles.TokenGlyph)
	assert.Contains(t, out, "8.0K")
}

// sidebarCostItem returns an assistant-message item carrying the given cost.
func sidebarCostItem(agentName string, cost float64) session.Item {
	return session.Item{Message: &session.Message{
		AgentName: agentName,
		Message:   chat.Message{Role: chat.MessageRoleAssistant, Content: "done", Cost: cost},
	}}
}

// TestComputeUsageStats_RestoredCostDoesNotDecreaseAfterCompaction is a
// regression test for the displayed total dropping after a session restore:
// LoadFromSession seeds the root entry with the restored total (own +
// embedded sub-session costs), and the next live TokenUsageEvent for the
// root — compaction emits one right away — used to replace it with an
// own-cost-only snapshot, shrinking the total.
func TestComputeUsageStats_RestoredCostDoesNotDecreaseAfterCompaction(t *testing.T) {
	t.Parallel()

	// Restored session: $0.10 direct + $0.05 embedded sub-session.
	sess := session.New()
	sess.InputTokens = 800
	sess.OutputTokens = 200
	sess.Messages = append(sess.Messages, sidebarCostItem("root", 0.10))
	sub := session.New(session.WithParentID(sess.ID))
	sub.Messages = append(sub.Messages, sidebarCostItem("developer", 0.05))
	sess.Messages = append(sess.Messages, session.Item{SubSession: sub})

	m := newTestSidebar(t)
	m.LoadFromSession(sess)
	assert.InDelta(t, 0.15, m.computeUsageStats().totalCost, 1e-9)

	// Compaction appends a $0.01 summary and immediately emits the session's
	// usage (see compactWithReason).
	sess.ApplyCompaction(100, 0, session.Item{Summary: "summary", Cost: 0.01})
	m.SetTokenUsage(&runtime.TokenUsageEvent{
		SessionID:    sess.ID,
		AgentContext: runtime.AgentContext{AgentName: "root"},
		Usage:        runtime.SessionUsage(sess, 100_000),
	})
	assert.InDelta(t, 0.16, m.computeUsageStats().totalCost, 1e-9,
		"the total must keep the embedded sub-session cost and gain the summary cost")
}

// TestComputeUsageStats_LiveSubSessionNotDoubleCounted drives the live
// delegation flow through the runtime's own usage snapshots: the child
// reports its own cost while it runs, and once it completes and attaches to
// the root the root's next event must not fold the child in again.
func TestComputeUsageStats_LiveSubSessionNotDoubleCounted(t *testing.T) {
	t.Parallel()

	usageEvent := func(sessionID, agentName string, sess *session.Session) *runtime.TokenUsageEvent {
		return &runtime.TokenUsageEvent{
			SessionID:    sessionID,
			AgentContext: runtime.AgentContext{AgentName: agentName},
			Usage:        runtime.SessionUsage(sess, 100_000),
		}
	}

	root := session.New()
	root.Messages = append(root.Messages, sidebarCostItem("root", 0.10))

	m := newTestSidebar(t)
	m.startStream(root.ID, "root")
	m.SetTokenUsage(usageEvent(root.ID, "root", root))

	child := session.New(session.WithParentID(root.ID))
	child.Messages = append(child.Messages, sidebarCostItem("developer", 0.05))
	m.startStream(child.ID, "developer")
	m.SetTokenUsage(usageEvent(child.ID, "developer", child))
	assert.InDelta(t, 0.15, m.computeUsageStats().totalCost, 1e-9)

	// Child completes: its stream stops and it is attached to the root.
	m.stopStream()
	root.AddLiveSubSession(child)

	m.SetTokenUsage(usageEvent(root.ID, "root", root))
	assert.InDelta(t, 0.15, m.computeUsageStats().totalCost, 1e-9,
		"$root own + $child own, not double-counted")
}
