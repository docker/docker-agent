package session

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/chat"
)

func assistantMsg(model string, cost float64, usage chat.Usage, createdAt string) *Message {
	return &Message{
		AgentName: "root",
		Message: chat.Message{
			Role:      chat.MessageRoleAssistant,
			Content:   "ok",
			Model:     model,
			Usage:     &usage,
			Cost:      cost,
			CreatedAt: createdAt,
		},
	}
}

func toolMsg(isError bool, createdAt string) *Message {
	return &Message{
		Message: chat.Message{
			Role:       chat.MessageRoleTool,
			Content:    "result",
			ToolCallID: "call_1",
			IsError:    isError,
			CreatedAt:  createdAt,
		},
	}
}

func TestStats_EmptySession(t *testing.T) {
	t.Parallel()

	st := New(WithID("sess-empty")).Stats()

	assert.Equal(t, "sess-empty", st.ID)
	assert.False(t, st.HasActivity())
	assert.Zero(t, st.Requests)
	assert.Zero(t, st.ToolCalls)
	assert.Zero(t, st.Cost)
	assert.Empty(t, st.Models)
	assert.Zero(t, st.SuccessRate())
	assert.Zero(t, st.CacheHitRate())
}

func TestStats_AggregatesTokensToolsAndModels(t *testing.T) {
	t.Parallel()

	sess := New(WithID("sess-1"))
	sess.AddMessage(UserMessage("hi"))
	sess.AddMessage(assistantMsg("cheap-model", 0.001, chat.Usage{
		InputTokens:  1000,
		OutputTokens: 200,
	}, ""))
	sess.AddMessage(toolMsg(false, ""))
	sess.AddMessage(toolMsg(true, ""))
	sess.AddMessage(assistantMsg("pricey-model", 0.05, chat.Usage{
		InputTokens:       500,
		CachedInputTokens: 1500,
		CacheWriteTokens:  100,
		OutputTokens:      300,
		ReasoningTokens:   80,
	}, ""))

	st := sess.Stats()

	assert.True(t, st.HasActivity())
	assert.Equal(t, 2, st.Requests)
	assert.Equal(t, 2, st.ToolCalls)
	assert.Equal(t, 1, st.ToolErrors)
	assert.Equal(t, 1, st.ToolSuccesses())
	assert.InDelta(t, 50.0, st.SuccessRate(), 0.001)

	assert.Equal(t, int64(1500), st.InputTokens)
	assert.Equal(t, int64(1500), st.CachedInput)
	assert.Equal(t, int64(100), st.CacheWrite)
	assert.Equal(t, int64(500), st.OutputTokens)
	assert.Equal(t, int64(80), st.ReasoningTokens)
	assert.Equal(t, int64(3100), st.TotalInput())
	assert.InDelta(t, 0.051, st.Cost, 1e-9)

	// 1500 cached out of (1500 new + 1500 cached) = 50%.
	assert.InDelta(t, 50.0, st.CacheHitRate(), 0.001)

	// Models sorted by cost descending.
	assert.Len(t, st.Models, 2)
	assert.Equal(t, "pricey-model", st.Models[0].Model)
	assert.Equal(t, "cheap-model", st.Models[1].Model)
	assert.Equal(t, 1, st.Models[0].Requests)
	assert.Equal(t, int64(1500), st.Models[0].CachedInput)
}

func TestStats_IncludesSubSessions(t *testing.T) {
	t.Parallel()

	sub := New(WithID("sub"))
	sub.AddMessage(assistantMsg("sub-model", 0.02, chat.Usage{
		InputTokens:  400,
		OutputTokens: 50,
	}, ""))
	sub.AddMessage(toolMsg(false, ""))

	parent := New(WithID("parent"))
	parent.AddMessage(assistantMsg("main-model", 0.01, chat.Usage{
		InputTokens:  100,
		OutputTokens: 20,
	}, ""))
	parent.AddSubSession(sub)

	st := parent.Stats()

	assert.Equal(t, 2, st.Requests)
	assert.Equal(t, 1, st.ToolCalls)
	assert.Equal(t, int64(500), st.InputTokens)
	assert.InDelta(t, 0.03, st.Cost, 1e-9)
	assert.Len(t, st.Models, 2)
}

func TestStats_Duration(t *testing.T) {
	t.Parallel()

	t0 := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)
	sess := New(WithID("sess-dur"))
	sess.AddMessage(assistantMsg("m", 0.001, chat.Usage{InputTokens: 10, OutputTokens: 5}, t0.Format(time.RFC3339)))
	sess.AddMessage(assistantMsg("m", 0.001, chat.Usage{InputTokens: 10, OutputTokens: 5}, t0.Add(90*time.Second).Format(time.RFC3339)))

	st := sess.Stats()

	assert.Equal(t, 90*time.Second, st.Duration)
}

func TestStats_RemoteFallback(t *testing.T) {
	t.Parallel()

	sess := New(WithID("sess-remote"))
	sess.AddMessageUsageRecord("root", "remote-model", 0.04, &chat.Usage{
		InputTokens:       200,
		CachedInputTokens: 600,
		OutputTokens:      90,
	})

	st := sess.Stats()

	assert.True(t, st.HasActivity())
	assert.Equal(t, 1, st.Requests)
	assert.Equal(t, int64(200), st.InputTokens)
	assert.Equal(t, int64(600), st.CachedInput)
	assert.InDelta(t, 0.04, st.Cost, 1e-9)
	assert.Len(t, st.Models, 1)
	assert.Equal(t, "remote-model", st.Models[0].Model)
}
