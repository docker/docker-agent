package runtime

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestSessionUsageCarriesCompactionThreshold(t *testing.T) {
	t.Parallel()

	sess := session.New(session.WithUserMessage("hi"))
	sess.InputTokens = 600
	sess.OutputTokens = 400

	u := SessionUsage(sess, 100_000)
	assert.Zero(t, u.CompactionThreshold, "threshold is 0 (unknown) when omitted")

	u = SessionUsage(sess, 100_000, 0.75)
	assert.InDelta(t, 0.75, u.CompactionThreshold, 0.0001)
	assert.Equal(t, int64(100_000), u.ContextLimit)
	assert.Equal(t, int64(1_000), u.ContextLength)
}

// costItem returns an assistant-message item carrying the given cost.
func costItem(agentName string, cost float64) session.Item {
	return session.Item{Message: &session.Message{
		AgentName: agentName,
		Message:   chat.Message{Role: chat.MessageRoleAssistant, Content: "done", Cost: cost},
	}}
}

// TestSessionUsageCostIncludesEmbeddedSubSessions reproduces the restored
// session shape: the root's direct cost plus an embedded sub-session that
// will never emit its own events. The emitted cost must include both, and a
// post-compaction emission (see compactWithReason) must increase it by the
// summary cost instead of dropping back to the root's own cost.
func TestSessionUsageCostIncludesEmbeddedSubSessions(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sess.Messages = append(sess.Messages, costItem("root", 0.10))
	sub := session.New(session.WithParentID(sess.ID))
	sub.Messages = append(sub.Messages, costItem("developer", 0.05))
	sess.Messages = append(sess.Messages, session.Item{SubSession: sub})

	u := SessionUsage(sess, 100_000)
	assert.InDelta(t, 0.15, u.Cost, 1e-9, "restored total = own + embedded sub-session cost")

	sess.ApplyCompaction(100, 0, session.Item{Summary: "summary", Cost: 0.01})
	u = SessionUsage(sess, 100_000)
	assert.InDelta(t, 0.16, u.Cost, 1e-9, "compaction increases the emitted total")
}

// TestSessionUsageCostExcludesLiveSubSessions pins the live aggregation
// contract: a sub-session that ran during this process reported its own cost
// through its own events, so attaching it to the parent must not inflate the
// parent's emitted cost.
func TestSessionUsageCostExcludesLiveSubSessions(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sess.Messages = append(sess.Messages, costItem("root", 0.10))

	child := session.New(session.WithParentID(sess.ID))
	child.Messages = append(child.Messages, costItem("developer", 0.05))
	assert.InDelta(t, 0.05, SessionUsage(child, 100_000).Cost, 1e-9, "the child emits its own cost")

	sess.AddLiveSubSession(child)
	assert.InDelta(t, 0.10, SessionUsage(sess, 100_000).Cost, 1e-9, "the parent keeps emitting only its own cost")
}

// TestBudgetExceededEventJSONContract pins the wire shape of
// budget_exceeded: the event embeds the assistant stop message under
// "stop_message" so JSON consumers (the evaluation pipeline reading
// `run --exec --json` output) can rebuild the message the runtime records
// right after the event. Only the safe representation crosses the wire:
// agent name plus role, content, and creation time. The source chat
// message deliberately carries sensitive fields to prove the dedicated
// [BudgetStopMessage] DTO cannot serialize them.
func TestBudgetExceededEventJSONContract(t *testing.T) {
	t.Parallel()

	const stopText = "Execution stopped after reaching the configured budget.max_cost limit (used $0.03 of $0.03)."
	stop := &chat.Message{
		Role:      chat.MessageRoleAssistant,
		Content:   stopText,
		CreatedAt: "2024-01-15T10:30:00Z",
		// Sensitive fields that must never reach the wire.
		ReasoningContent: "secret reasoning",
		ToolCalls: []tools.ToolCall{{
			ID:       "call-1",
			Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"cat /etc/passwd"}`},
		}},
		Model: "openai/gpt-4o",
		Usage: &chat.Usage{InputTokens: 42},
		Cost:  0.03,
	}
	breach := budgetBreach{Budget: "run", Limit: budgetLimitCost, Used: "$0.03", Max: "$0.03"}

	data, err := json.Marshal(BudgetExceeded("sess-1", "root", breach, stop))
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(data, &decoded))
	assert.Equal(t, "budget_exceeded", decoded["type"])
	assert.Equal(t, "sess-1", decoded["session_id"])
	assert.Equal(t, "budget.max_cost", decoded["config_path"])

	payload, ok := decoded["stop_message"].(map[string]any)
	require.True(t, ok, "stop_message payload must be serialized")
	assert.Equal(t, "root", payload["agent_name"])
	inner, ok := payload["message"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "assistant", inner["role"])
	assert.Equal(t, stopText, inner["content"])
	assert.Equal(t, "2024-01-15T10:30:00Z", inner["created_at"])

	// Safety: nothing beyond the allow-listed keys is serialized, so the
	// source's reasoning, tool calls, model, usage, and cost cannot pass.
	for key := range payload {
		assert.Contains(t, []string{"agent_name", "message"}, key)
	}
	for key := range inner {
		assert.Contains(t, []string{"role", "content", "created_at"}, key)
	}
	assert.NotContains(t, string(data), "secret reasoning")
	assert.NotContains(t, string(data), "/etc/passwd")
	assert.NotContains(t, string(data), "gpt-4o")

	// The client-side decoder (see NewClient's registry) restores it.
	var event BudgetExceededEvent
	require.NoError(t, json.Unmarshal(data, &event))
	require.NotNil(t, event.StopMessage)
	assert.Equal(t, stopText, event.StopMessage.Message.Content)

	// Without a stop message the key is omitted.
	data, err = json.Marshal(BudgetExceeded("sess-1", "root", breach, nil))
	require.NoError(t, err)
	assert.NotContains(t, string(data), "stop_message")
}

// TestMessageAddedEventJSONHasNoPayload pins that message_added never
// serializes the message it carries: the payload exists only for
// in-process consumers such as the PersistentRuntime wrapper. The budget
// stop message reaches JSON consumers through
// [BudgetExceededEvent.StopMessage] instead.
func TestMessageAddedEventJSONHasNoPayload(t *testing.T) {
	t.Parallel()

	msg := session.NewAgentMessage("root", &chat.Message{
		Role:      chat.MessageRoleAssistant,
		Content:   "in-process only",
		CreatedAt: "2024-01-15T10:30:00Z",
	})

	data, err := json.Marshal(MessageAdded("sess-1", msg, "root"))
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(data, &decoded))
	assert.Equal(t, "message_added", decoded["type"])
	assert.Equal(t, "sess-1", decoded["session_id"])
	assert.NotContains(t, decoded, "message")
	assert.NotContains(t, string(data), "in-process only")
}

func TestMessageAddedCompletionMetadataJSON(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		msg   chat.Message
		empty bool
		tools bool
	}{
		{"answer", chat.Message{Role: chat.MessageRoleAssistant, Content: "answer"}, false, false},
		{"empty", chat.Message{Role: chat.MessageRoleAssistant}, true, false},
		{"tools", chat.Message{Role: chat.MessageRoleAssistant, Content: "preamble", ToolCalls: []tools.ToolCall{{ID: "call"}}}, false, true},
		{"user", chat.Message{Role: chat.MessageRoleUser}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			msg := session.NewAgentMessage("root", &tc.msg)
			data, err := json.Marshal(MessageAdded("sess", msg, "root"))
			require.NoError(t, err)
			var decoded MessageAddedEvent
			require.NoError(t, json.Unmarshal(data, &decoded))
			assert.Equal(t, tc.empty, decoded.AssistantMessageEmpty)
			assert.Equal(t, tc.tools, decoded.HasToolCalls)
			assert.Nil(t, decoded.Message, "message content remains off the wire")
		})
	}
}
