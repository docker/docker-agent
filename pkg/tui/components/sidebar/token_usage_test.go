package sidebar

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestCurrentSessionTokens_SingleSession(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	m := New(sessionState).(*model)

	m.SetTokenUsage(&runtime.TokenUsageEvent{
		SessionID:    "session-1",
		AgentContext: runtime.AgentContext{AgentName: "root"},
		Usage: &runtime.Usage{
			InputTokens:  5000,
			OutputTokens: 3000,
		},
	})

	m.currentAgent = "root"
	tokens, found := m.currentSessionTokens()
	assert.True(t, found)
	assert.Equal(t, int64(8000), tokens)
}

func TestCurrentSessionTokens_MultipleSessions(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	m := New(sessionState).(*model)

	m.SetTokenUsage(&runtime.TokenUsageEvent{
		SessionID:    "session-root",
		AgentContext: runtime.AgentContext{AgentName: "root"},
		Usage: &runtime.Usage{
			InputTokens:  20000,
			OutputTokens: 10000,
		},
	})

	m.SetTokenUsage(&runtime.TokenUsageEvent{
		SessionID:    "session-child",
		AgentContext: runtime.AgentContext{AgentName: "developer"},
		Usage: &runtime.Usage{
			InputTokens:  8000,
			OutputTokens: 2000,
		},
	})

	// Current agent is developer — should return developer's tokens
	m.currentAgent = "developer"
	m.currentSessionID = "session-child"
	tokens, found := m.currentSessionTokens()
	assert.True(t, found)
	assert.Equal(t, int64(10000), tokens)

	// Switch to root — should return root's tokens
	m.currentAgent = "root"
	m.currentSessionID = "session-root"
	tokens, found = m.currentSessionTokens()
	assert.True(t, found)
	assert.Equal(t, int64(30000), tokens)
}

func TestCurrentSessionTokens_FallbackToSingleSession(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	m := New(sessionState).(*model)

	m.sessionUsage["session-1"] = &runtime.Usage{
		InputTokens:  5000,
		OutputTokens: 5000,
	}

	tokens, found := m.currentSessionTokens()
	assert.True(t, found)
	assert.Equal(t, int64(10000), tokens)
}

func TestCurrentSessionTokens_Empty(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	m := New(sessionState).(*model)

	tokens, found := m.currentSessionTokens()
	assert.False(t, found)
	assert.Equal(t, int64(0), tokens)
}

func TestCurrentSessionTokens_SessionIDTakesPrecedence(t *testing.T) {
	t.Parallel()

	// This test verifies the fix for the flickering bug.
	// When the same agent runs multiple sessions (e.g., transfer_task called
	// twice to the same sub-agent, or new_session for the same agent), the
	// agent name lookup in sessionAgent is ambiguous because Go map iteration
	// order is non-deterministic. Using currentSessionID for direct lookup
	// eliminates this ambiguity.

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	m := New(sessionState).(*model)

	// Same agent "developer" has two sessions (e.g., transfer_task called twice)
	m.SetTokenUsage(&runtime.TokenUsageEvent{
		SessionID:    "child-session-1",
		AgentContext: runtime.AgentContext{AgentName: "developer"},
		Usage: &runtime.Usage{
			InputTokens:  5000,
			OutputTokens: 5000,
		},
	})

	m.SetTokenUsage(&runtime.TokenUsageEvent{
		SessionID:    "child-session-2",
		AgentContext: runtime.AgentContext{AgentName: "developer"},
		Usage: &runtime.Usage{
			InputTokens:  20000,
			OutputTokens: 10000,
		},
	})

	m.currentAgent = "developer"
	m.currentSessionID = "child-session-2"

	// Must consistently return session-2's tokens, not randomly pick between them
	for range 100 {
		tokens, found := m.currentSessionTokens()
		assert.True(t, found)
		assert.Equal(t, int64(30000), tokens, "currentSessionTokens() returned inconsistent value — the flickering bug is back")
	}
}

func TestTokenUsageSummary_SingleSession(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	m := New(sessionState).(*model)

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

	m.currentAgent = "root"
	m.currentSessionID = "session-1"
	summary := m.tokenUsageSummary()
	// Single session: shows total tokens, cost, and context
	assert.Contains(t, summary, "Tokens: 8.0K")
	assert.Contains(t, summary, "Cost: $0.05")
	assert.Contains(t, summary, "Context: 8%")
	assert.NotContains(t, summary, "sub-sessions")
}

func TestTokenUsageSummary_MultipleSessions_ShowsCurrentSessionTokens(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	m := New(sessionState).(*model)

	// Root agent session: 30K tokens, $0.10
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

	m.currentAgent = "root"
	m.currentSessionID = "session-root"
	summary := m.tokenUsageSummary()
	// Should show current session (root) tokens, not total
	assert.Contains(t, summary, "Tokens: 30.0K")
	// Should show total cost ($0.10 + $0.05)
	assert.Contains(t, summary, "Cost: $0.15")
	// Should show context % for root session
	assert.Contains(t, summary, "Context: 30%")
	// Should show sub-session count
	assert.Contains(t, summary, "1 sub-sessions")

	// Switch to developer
	m.currentAgent = "developer"
	m.currentSessionID = "session-child"
	summary = m.tokenUsageSummary()
	// Should show current session (developer) tokens
	assert.Contains(t, summary, "Tokens: 10.0K")
	// Should still show total cost
	assert.Contains(t, summary, "Cost: $0.15")
	// Should show context % for developer session
	assert.Contains(t, summary, "Context: 5%")
}

func TestTokenUsageSummary_Empty(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	m := New(sessionState).(*model)

	assert.Empty(t, m.tokenUsageSummary())
}

func TestContextPercent_SessionIDTakesPrecedence(t *testing.T) {
	t.Parallel()

	// Same flickering bug applies to contextPercent — verify session ID lookup
	// takes precedence over non-deterministic agent name map iteration.

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	m := New(sessionState).(*model)

	m.SetTokenUsage(&runtime.TokenUsageEvent{
		SessionID:    "child-session-1",
		AgentContext: runtime.AgentContext{AgentName: "developer"},
		Usage: &runtime.Usage{
			ContextLength: 10000,
			ContextLimit:  100000,
		},
	})

	m.SetTokenUsage(&runtime.TokenUsageEvent{
		SessionID:    "child-session-2",
		AgentContext: runtime.AgentContext{AgentName: "developer"},
		Usage: &runtime.Usage{
			ContextLength: 50000,
			ContextLimit:  100000,
		},
	})

	m.currentAgent = "developer"
	m.currentSessionID = "child-session-2"

	for range 100 {
		assert.Equal(t, "50%", m.contextPercent(), "contextPercent() returned inconsistent value — the flickering bug is back")
	}
}
