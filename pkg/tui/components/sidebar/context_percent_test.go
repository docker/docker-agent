package sidebar

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestContextPercent_SingleSession(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	m := New(sessionState).(*model)

	m.SetTokenUsage(&runtime.TokenUsageEvent{
		SessionID:    "session-1",
		AgentContext: runtime.AgentContext{AgentName: "root"},
		Usage: &runtime.Usage{
			InputTokens:   5000,
			OutputTokens:  5000,
			ContextLength: 10000,
			ContextLimit:  100000,
		},
	})

	assert.Equal(t, "10%", m.contextPercent())
}

func TestContextPercent_MultipleSessionsWithCurrentAgent(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	m := New(sessionState).(*model)

	// Root agent session
	m.SetTokenUsage(&runtime.TokenUsageEvent{
		SessionID:    "session-root",
		AgentContext: runtime.AgentContext{AgentName: "root"},
		Usage: &runtime.Usage{
			InputTokens:   20000,
			OutputTokens:  10000,
			ContextLength: 30000,
			ContextLimit:  100000,
		},
	})

	// Child agent session (from transfer_task)
	m.SetTokenUsage(&runtime.TokenUsageEvent{
		SessionID:    "session-child",
		AgentContext: runtime.AgentContext{AgentName: "developer"},
		Usage: &runtime.Usage{
			InputTokens:   8000,
			OutputTokens:  2000,
			ContextLength: 10000,
			ContextLimit:  200000,
		},
	})

	// Current agent is the child — should show child's percentage
	m.currentAgent = "developer"
	assert.Equal(t, "5%", m.contextPercent())

	// Switch back to root agent — should show root's percentage
	m.currentAgent = "root"
	assert.Equal(t, "30%", m.contextPercent())
}

func TestContextPercent_NoContextLimit(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	m := New(sessionState).(*model)

	m.SetTokenUsage(&runtime.TokenUsageEvent{
		SessionID:    "session-1",
		AgentContext: runtime.AgentContext{AgentName: "root"},
		Usage: &runtime.Usage{
			InputTokens:   5000,
			OutputTokens:  5000,
			ContextLength: 10000,
			ContextLimit:  0, // No context limit
		},
	})

	m.currentAgent = "root"
	assert.Empty(t, m.contextPercent())
}

func TestContextPercent_EmptyUsage(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	m := New(sessionState).(*model)

	assert.Empty(t, m.contextPercent())
}

func TestContextPercent_FallbackToSingleSession(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	m := New(sessionState).(*model)

	// Session with no agent mapping (e.g., restored from persistence)
	m.sessionUsage["session-1"] = &runtime.Usage{
		InputTokens:   5000,
		OutputTokens:  5000,
		ContextLength: 10000,
		ContextLimit:  100000,
	}
	// No sessionAgent entry, no currentAgent set

	assert.Equal(t, "10%", m.contextPercent())
}

// TestContextPercent_StaleSessionIDAfterSubAgent verifies that contextPercent()
// returns the correct value throughout a transfer_task round-trip.
//
// After a sub-agent's stream stops, the parent's AgentInfo event restores
// currentAgent to the parent while currentSessionID still references the child
// session. The sidebar must detect this stale ID and fall back to an
// agent-name lookup so the displayed context % matches the parent.
func TestContextPercent_StaleSessionIDAfterSubAgent(t *testing.T) {
	t.Parallel()

	m := newTestSidebar()

	// Parent starts.
	m.setAgent("root")
	m.startStream("parent-session", "root")
	m.recordUsage("parent-session", "root", 30000, 100000)
	assert.Equal(t, "30%", m.contextPercent(), "parent at 30%%")

	// --- transfer_task to "developer" ---
	m.setAgent("developer")
	m.startStream("child-session-1", "developer")
	m.recordUsage("child-session-1", "developer", 10000, 200000)
	assert.Equal(t, "5%", m.contextPercent(), "developer sub-agent at 5%%")

	m.stopStream()
	m.setAgent("root") // parent restored

	// Key assertion: stale currentSessionID must not cause a wrong lookup.
	assert.Equal(t, "30%", m.contextPercent(),
		"after sub-agent returns, context %% must reflect the parent (30%%), not the child (5%%)")

	// --- transfer_task to "researcher" (second round-trip) ---
	m.setAgent("researcher")
	m.startStream("child-session-2", "researcher")
	m.recordUsage("child-session-2", "researcher", 80000, 100000)
	assert.Equal(t, "80%", m.contextPercent(), "researcher sub-agent at 80%%")

	m.stopStream()
	m.setAgent("root") // parent restored again

	assert.Equal(t, "30%", m.contextPercent(),
		"after second sub-agent returns, context %% must still reflect the parent (30%%)")

	// Parent resumes with a new stream iteration.
	m.startStream("parent-session", "root")
	m.recordUsage("parent-session", "root", 40000, 100000)
	assert.Equal(t, "40%", m.contextPercent(), "parent resumes at 40%%")
}

// testSidebar wraps *model with helpers that mirror the sidebar field mutations
// performed by Update() for each runtime event — without touching the global
// spinner/animation coordinator, which would leak state across test runs.
type testSidebar struct {
	*model
}

func newTestSidebar() *testSidebar {
	sess := session.New()
	return &testSidebar{
		model: New(service.NewSessionState(sess)).(*model),
	}
}

func (s *testSidebar) setAgent(name string) {
	s.currentAgent = name
}

func (s *testSidebar) startStream(sessionID, agentName string) {
	s.workingAgent = agentName
	s.currentSessionID = sessionID
}

func (s *testSidebar) stopStream() {
	s.workingAgent = ""
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
