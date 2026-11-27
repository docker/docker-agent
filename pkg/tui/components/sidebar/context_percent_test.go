package sidebar

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestContextPercent_SingleSession(t *testing.T) {
	t.Parallel()

	m := newTestSidebar(t)
	m.startStream("session-1", "root")
	m.recordUsage("session-1", "root", 10000, 100000)

	assert.Equal(t, "10%", m.contextPercent())
}

// TestContextPercent_TracksActiveSession verifies the percentage follows the
// active session: the sub-agent's context while it runs, the parent's once it
// returns.
func TestContextPercent_TracksActiveSession(t *testing.T) {
	t.Parallel()

	m := newTestSidebar(t)

	m.startStream("session-root", "root")
	m.recordUsage("session-root", "root", 30000, 100000)

	m.startStream("session-child", "developer")
	m.recordUsage("session-child", "developer", 10000, 200000)
	assert.Equal(t, "5%", m.contextPercent(), "sub-agent context while it runs")

	m.stopStream()
	assert.Equal(t, "30%", m.contextPercent(), "parent context after the sub-agent returns")
}

func TestContextPercent_NoContextLimit(t *testing.T) {
	t.Parallel()

	m := newTestSidebar(t)
	m.startStream("session-1", "root")
	m.recordUsage("session-1", "root", 10000, 0) // No context limit

	assert.Empty(t, m.contextPercent())
}

func TestContextPercent_EmptyUsage(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	m := New(animation.NewRuntime(), t.Context(), sessionState).(*model)

	assert.Empty(t, m.contextPercent())
}

func TestContextPercent_FallbackToSingleSession(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	m := New(animation.NewRuntime(), t.Context(), sessionState).(*model)

	// Session with no active stream (e.g., restored from persistence)
	m.sessionUsage["session-1"] = &sessionUsage{Usage: runtime.Usage{
		InputTokens:   5000,
		OutputTokens:  5000,
		ContextLength: 10000,
		ContextLimit:  100000,
	}}

	assert.Equal(t, "10%", m.contextPercent())
}

// TestContextPercent_StableDuringSubAgent verifies the percentage does not
// flicker while a sub-agent is active: it consistently reflects the
// sub-session, independent of which agent last emitted an event.
func TestContextPercent_StableDuringSubAgent(t *testing.T) {
	t.Parallel()

	m := newTestSidebar(t)

	m.startStream("session-root", "root")
	m.recordUsage("session-root", "root", 30000, 100000)

	m.startStream("session-child", "developer")
	m.recordUsage("session-child", "developer", 50000, 100000)

	m.currentAgent = "root"
	for range 100 {
		assert.Equal(t, "50%", m.contextPercent(), "contextPercent() flickered while a sub-agent was running")
	}
}

// TestContextPercent_RoundTripDelegations replays two transfer_task round-trips
// and asserts the percentage tracks the active session at every step.
func TestContextPercent_RoundTripDelegations(t *testing.T) {
	t.Parallel()

	m := newTestSidebar(t)

	// Parent starts.
	m.startStream("parent-session", "root")
	m.recordUsage("parent-session", "root", 30000, 100000)
	assert.Equal(t, "30%", m.contextPercent(), "parent at 30%%")

	// --- transfer_task to "developer" (nested stream) ---
	m.startStream("child-session-1", "developer")
	m.recordUsage("child-session-1", "developer", 10000, 200000)
	assert.Equal(t, "5%", m.contextPercent(), "developer sub-agent at 5%%")

	m.stopStream()
	assert.Equal(t, "30%", m.contextPercent(), "after sub-agent returns, back to the parent (30%%)")

	// --- transfer_task to "researcher" (second round-trip) ---
	m.startStream("child-session-2", "researcher")
	m.recordUsage("child-session-2", "researcher", 80000, 100000)
	assert.Equal(t, "80%", m.contextPercent(), "researcher sub-agent at 80%%")

	m.stopStream()
	assert.Equal(t, "30%", m.contextPercent(), "after second sub-agent returns, back to the parent (30%%)")

	// Parent resumes with more usage on the main session.
	m.recordUsage("parent-session", "root", 40000, 100000)
	assert.Equal(t, "40%", m.contextPercent(), "parent resumes at 40%%")

	// Parent's outermost stream stops; the main session remains the fallback.
	m.stopStream()
	assert.Equal(t, "40%", m.contextPercent(), "idle: still the main session")
}

// testSidebar wraps *model with helpers that mirror the sidebar field mutations
// performed by Update() for each runtime event — without touching the global
// spinner/animation coordinator, which would leak state across test runs.
type testSidebar struct {
	*model
}

func newTestSidebar(tb testing.TB) *testSidebar {
	tb.Helper()
	sess := session.New()
	return &testSidebar{
		model: New(animation.NewRuntime(), testContext(tb), service.NewSessionState(sess)).(*model),
	}
}

func (s *testSidebar) startStream(sessionID, agentName string) {
	s.currentAgent = agentName
	s.workingAgent = agentName
	if len(s.sessionStack) == 0 {
		s.rootSessionID = sessionID
	}
	s.sessionStack = append(s.sessionStack, streamIdentity{sessionID, agentName})
}

func (s *testSidebar) stopStream() {
	s.workingAgent = ""
	if n := len(s.sessionStack); n > 0 {
		s.sessionStack = s.sessionStack[:n-1]
	}
}

func (s *testSidebar) recordUsage(sessionID, agentName string, contextLen, contextLimit int64) {
	s.SetTokenUsage(&runtime.TokenUsageEvent{
		SessionID:    sessionID,
		AgentContext: runtime.AgentContext{AgentName: agentName},
		Usage: &runtime.Usage{
			InputTokens:   contextLen / 2,
			OutputTokens:  contextLen / 2,
			ContextLength: contextLen,
			ContextLimit:  contextLimit,
		},
	})
}

func (s *testSidebar) recordUsageTokens(sessionID, agentName string, input, output int64) {
	s.SetTokenUsage(&runtime.TokenUsageEvent{
		SessionID:    sessionID,
		AgentContext: runtime.AgentContext{AgentName: agentName},
		Usage: &runtime.Usage{
			InputTokens:  input,
			OutputTokens: output,
		},
	})
}
