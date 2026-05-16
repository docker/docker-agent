package dialog

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestNewCostDialog(t *testing.T) {
	t.Parallel()

	sess := session.New()

	dialog := NewCostDialog(sess)

	require.NotNil(t, dialog)
}

func TestCostDialogView(t *testing.T) {
	t.Parallel()

	sess := session.New()

	// Add some messages with usage info
	sess.AddMessage(&session.Message{
		AgentName: "root",
		Message: chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "Hello",
			Model:   "gpt-4o",
			Usage: &chat.Usage{
				InputTokens:  1000,
				OutputTokens: 500,
			},
			Cost: 0.005,
		},
	})

	sess.AddMessage(&session.Message{
		AgentName: "root",
		Message: chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "World",
			Model:   "gpt-4o",
			Usage: &chat.Usage{
				InputTokens:       800,
				OutputTokens:      300,
				CachedInputTokens: 200,
			},
			Cost: 0.003,
		},
	})

	dialog := NewCostDialog(sess)
	// Set a large enough window size
	dialog.SetSize(100, 50)
	view := dialog.View()

	// Check that the view contains expected content
	// The title may be split across lines due to narrow width
	assert.Contains(t, view, "Session Cost")
	assert.Contains(t, view, "Total")
	assert.Contains(t, view, "By Model")
	assert.Contains(t, view, "gpt-4o")
	assert.Contains(t, view, "tokens:")           // total token count line
	assert.Contains(t, view, "messages:")         // message count in header
	assert.Contains(t, view, "avg cost/message:") // average cost per message
}

func TestCostDialogWithToolCalls(t *testing.T) {
	t.Parallel()

	sess := session.New()

	// Add message with tool calls
	sess.AddMessage(&session.Message{
		AgentName: "root",
		Message: chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "Let me help you",
			Model:   "claude-sonnet-4-0",
			ToolCalls: []tools.ToolCall{
				{ID: "call_1", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"ls"}`}},
			},
			Usage: &chat.Usage{
				InputTokens:  2000,
				OutputTokens: 100,
			},
			Cost: 0.01,
		},
	})

	dialog := NewCostDialog(sess)
	// Set a large enough window size
	dialog.SetSize(100, 50)
	view := dialog.View()

	// Model name may be split across lines
	assert.Contains(t, view, "claude")
	assert.Contains(t, view, "$0.01")
}

func TestCostDialogWithReasoningTokens(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sess.AddMessage(&session.Message{
		AgentName: "root",
		Message: chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "Thought deeply",
			Model:   "o1",
			Usage: &chat.Usage{
				InputTokens:     500,
				OutputTokens:    200,
				ReasoningTokens: 1500,
			},
			Cost: 0.01,
		},
	})

	dialog := NewCostDialog(sess)
	dialog.SetSize(100, 50)
	view := dialog.View()

	assert.Contains(t, view, "reasoning:")
	assert.Contains(t, view, "1.5K")
}

func TestCostDialogAvgCostPerToken(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sess.AddMessage(&session.Message{
		AgentName: "root",
		Message: chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "Hello",
			Model:   "gpt-4o",
			Usage: &chat.Usage{
				InputTokens:  1000,
				OutputTokens: 1000,
			},
			Cost: 0.10,
		},
	})

	dialog := NewCostDialog(sess)
	dialog.SetSize(100, 50)
	view := dialog.View()

	// 0.10 / 2000 * 1000 = 0.05 per 1K tokens
	assert.Contains(t, view, "avg cost/1K tokens:")
}

func TestCostDialogModelPercentage(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sess.AddMessage(&session.Message{
		AgentName: "root",
		Message: chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "Expensive",
			Model:   "gpt-4o",
			Usage:   &chat.Usage{InputTokens: 1000, OutputTokens: 500},
			Cost:    0.75,
		},
	})
	sess.AddMessage(&session.Message{
		AgentName: "root",
		Message: chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "Cheap",
			Model:   "gpt-4o-mini",
			Usage:   &chat.Usage{InputTokens: 100, OutputTokens: 50},
			Cost:    0.25,
		},
	})

	dialog := NewCostDialog(sess)
	dialog.SetSize(120, 50)
	view := dialog.View()

	// gpt-4o should show 75%, gpt-4o-mini 25%
	assert.Contains(t, view, "75%")
	assert.Contains(t, view, "25%")
}

func TestCostDialogCacheHitRate(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sess.AddMessage(&session.Message{
		AgentName: "root",
		Message: chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "Cached result",
			Model:   "gpt-4o",
			Usage: &chat.Usage{
				InputTokens:       300,
				CachedInputTokens: 700,
				OutputTokens:      100,
			},
			Cost: 0.01,
		},
	})

	dialog := NewCostDialog(sess)
	dialog.SetSize(130, 50)
	view := dialog.View()

	// 700 cached out of 1000 total input = 70% hit rate
	assert.Contains(t, view, "cache hit rate:")
	assert.Contains(t, view, "70%")

	// By Model line should also show cached token count
	assert.Contains(t, view, "cached:")
}

func TestCostDialogCacheHitRateWithCacheWriteTokens(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sess.AddMessage(&session.Message{
		AgentName: "root",
		Message: chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "Cached result with write tokens",
			Model:   "gpt-4o",
			Usage: &chat.Usage{
				InputTokens:       300,
				CachedInputTokens: 700,
				CacheWriteTokens:  200,
				OutputTokens:      100,
			},
			Cost: 0.01,
		},
	})

	data := (&costDialog{session: sess}).gatherCostData()
	stats := data.totalStats()

	// Cache hit rate should be 700/(700+300) = 70%, NOT 700/(700+300+200) = 58%.
	// CacheWriteTokens must NOT be included in the denominator.
	var cacheHitRate string
	for _, s := range stats {
		if s.label == "cache hit rate:" {
			cacheHitRate = s.value
		}
	}
	assert.Equal(t, "70%", cacheHitRate)
}

func TestCostDialogEmptySession(t *testing.T) {
	t.Parallel()

	sess := session.New()

	dialog := NewCostDialog(sess)
	// Set a large enough window size
	dialog.SetSize(100, 50)
	view := dialog.View()

	// Should still render without errors
	assert.Contains(t, view, "Session Cost")
	assert.Contains(t, view, "Total")
	assert.Contains(t, view, "$0.00") // Zero cost
}

func TestCostDialogWithCompactionCost(t *testing.T) {
	t.Parallel()

	sess := session.New()

	// Add a regular message with usage
	sess.AddMessage(&session.Message{
		AgentName: "root",
		Message: chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "Hello",
			Model:   "gpt-4o",
			Usage: &chat.Usage{
				InputTokens:  1000,
				OutputTokens: 500,
			},
			Cost: 0.005,
		},
	})

	// Add a compaction summary item with cost (simulates what session_compaction.go does)
	sess.Messages = append(sess.Messages, session.Item{
		Summary: "This is a session summary after compaction.",
		Cost:    0.003,
	})

	data := (&costDialog{session: sess}).gatherCostData()

	// Total cost should include both the message cost and the compaction cost
	assert.InDelta(t, 0.008, data.total.cost, 0.0001)

	// There should be 2 entries in the per-message breakdown:
	// one for the assistant message and one for compaction
	require.Len(t, data.messages, 2)
	assert.InDelta(t, 0.005, data.messages[0].cost, 0.0001)
	assert.Equal(t, "compaction", data.messages[1].label)
	assert.InDelta(t, 0.003, data.messages[1].cost, 0.0001)
}

func TestCostDialogCompactionCostRendersInView(t *testing.T) {
	t.Parallel()

	sess := session.New()

	sess.AddMessage(&session.Message{
		AgentName: "root",
		Message: chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "Hello",
			Model:   "gpt-4o",
			Usage: &chat.Usage{
				InputTokens:  1000,
				OutputTokens: 500,
			},
			Cost: 0.005,
		},
	})

	sess.Messages = append(sess.Messages, session.Item{
		Summary: "Session summary.",
		Cost:    0.002,
	})

	dialog := NewCostDialog(sess)
	dialog.SetSize(100, 50)
	view := dialog.View()

	assert.Contains(t, view, "compaction")
	assert.Contains(t, view, "$0.0070") // total: 0.005 + 0.002
}

func TestCostDialogWithSubSessions(t *testing.T) {
	t.Parallel()

	sess := session.New()

	// Add a parent message with usage
	sess.AddMessage(&session.Message{
		AgentName: "root",
		Message: chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "Let me create a sub-session",
			Model:   "gpt-4o",
			Usage: &chat.Usage{
				InputTokens:  1000,
				OutputTokens: 200,
			},
			Cost: 0.005,
		},
	})

	// Create a sub-session with its own messages
	subSess := session.New()
	subSess.AddMessage(&session.Message{
		AgentName: "sub-agent",
		Message: chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "Working on it",
			Model:   "gpt-4o-mini",
			Usage: &chat.Usage{
				InputTokens:  500,
				OutputTokens: 100,
			},
			Cost: 0.001,
		},
	})
	subSess.AddMessage(&session.Message{
		AgentName: "sub-agent",
		Message: chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "Done!",
			Model:   "gpt-4o-mini",
			Usage: &chat.Usage{
				InputTokens:  600,
				OutputTokens: 150,
			},
			Cost: 0.002,
		},
	})

	sess.AddSubSession(subSess)

	// Gather cost data
	data := (&costDialog{session: sess}).gatherCostData()

	// Total cost should include parent + sub-session messages
	assert.InDelta(t, 0.008, data.total.cost, 0.0001)

	// Messages should include: parent msg, sub-session start marker, 2 sub-session msgs, sub-session end marker
	require.Len(t, data.messages, 5)
	assert.Equal(t, "#1 [root]", data.messages[0].label)
	assert.True(t, data.messages[1].isSubSessionMarker(), "expected sub-session start marker")
	assert.Contains(t, data.messages[1].label, "sub-session start")
	assert.Equal(t, "#2 [sub-agent]", data.messages[2].label)
	assert.Equal(t, "#3 [sub-agent]", data.messages[3].label)
	assert.True(t, data.messages[4].isSubSessionMarker(), "expected sub-session end marker")
	assert.Contains(t, data.messages[4].label, "sub-session end")
	assert.Contains(t, data.messages[4].label, "$0.0030") // sub-session total cost
}

func TestCostDialogSubSessionRendersInView(t *testing.T) {
	t.Parallel()

	sess := session.New()

	sess.AddMessage(&session.Message{
		AgentName: "root",
		Message: chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "Creating sub-session",
			Model:   "gpt-4o",
			Usage: &chat.Usage{
				InputTokens:  1000,
				OutputTokens: 200,
			},
			Cost: 0.005,
		},
	})

	subSess := session.New()
	subSess.AddMessage(&session.Message{
		AgentName: "sub-agent",
		Message: chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "Sub result",
			Model:   "gpt-4o-mini",
			Usage: &chat.Usage{
				InputTokens:  400,
				OutputTokens: 80,
			},
			Cost: 0.001,
		},
	})
	sess.AddSubSession(subSess)

	dialog := NewCostDialog(sess)
	dialog.SetSize(100, 50)
	view := dialog.View()

	assert.Contains(t, view, "sub-session start")
	assert.Contains(t, view, "sub-session end")
	assert.Contains(t, view, "sub-agent")
}

func TestFormatCost(t *testing.T) {
	t.Parallel()

	tests := []struct {
		cost     float64
		expected string
	}{
		{0.0, "$0.00"},
		{0.00001, "$0.00"},
		{0.0001, "$0.0001"},
		{0.001, "$0.0010"},
		{0.01, "$0.01"},
		{0.1, "$0.10"},
		{1.0, "$1.00"},
		{10.5, "$10.50"},
	}

	for _, tt := range tests {
		result := formatCost(tt.cost)
		assert.Equal(t, tt.expected, result, "formatCost(%f)", tt.cost)
	}
}

func TestFormatCostNegative(t *testing.T) {
	t.Parallel()

	// Negative costs should format with a leading "-" prefix.
	assert.Equal(t, "-$0.01", formatCost(-0.01))
	assert.Equal(t, "-$0.0050", formatCost(-0.005))
	assert.Equal(t, "-$1.00", formatCost(-1.0))
	// Very small negative is clamped to zero.
	assert.Equal(t, "-$0.00", formatCost(-0.00001))
}

func TestFormatTokenCount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		count    int64
		expected string
	}{
		{0, "0"},
		{100, "100"},
		{999, "999"},
		{1000, "1.0K"},
		{1500, "1.5K"},
		{10000, "10.0K"},
		{999999, "1000.0K"},
		{1000000, "1.0M"},
		{1500000, "1.5M"},
		{10000000, "10.0M"},
	}

	for _, tt := range tests {
		result := formatTokenCount(tt.count)
		assert.Equal(t, tt.expected, result, "formatTokenCount(%d)", tt.count)
	}
}

func TestFormatDuration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		d        time.Duration
		expected string
	}{
		{-5 * time.Second, "0s"},  // negative durations clamp to 0
		{-90 * time.Second, "0s"}, // negative durations clamp to 0
		{0, "0s"},
		{30 * time.Second, "30s"},
		{59 * time.Second, "59s"},
		{60 * time.Second, "1m"},
		{90 * time.Second, "1m 30s"},
		{5 * time.Minute, "5m"},
		{60 * time.Minute, "1h"},
		{90 * time.Minute, "1h 30m"},
		{2*time.Hour + 15*time.Minute, "2h 15m"},
	}

	for _, tt := range tests {
		result := formatDuration(tt.d)
		assert.Equal(t, tt.expected, result, "formatDuration(%v)", tt.d)
	}
}
