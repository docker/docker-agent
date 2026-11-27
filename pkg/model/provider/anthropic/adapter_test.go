package anthropic

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
)

func TestUsageFromMessage_RecordsReasoningTokens(t *testing.T) {
	t.Parallel()
	u := Usage{
		InputTokens:              100,
		OutputTokens:             80,
		CacheReadInputTokens:     20,
		CacheCreationInputTokens: 10,
		OutputTokensDetails:      OutputTokensDetails{ThinkingTokens: 55},
	}

	got := usageFromMessage(u)

	require.NotNil(t, got)
	assert.Equal(t, int64(100), got.InputTokens)
	assert.Equal(t, int64(80), got.OutputTokens)
	assert.Equal(t, int64(20), got.CachedInputTokens)
	assert.Equal(t, int64(10), got.CacheWriteTokens)
	assert.Equal(t, int64(55), got.ReasoningTokens,
		"thinking tokens must be surfaced as ReasoningTokens, not dropped")
}

func TestBetaUsageFromMessage_RecordsReasoningTokens(t *testing.T) {
	t.Parallel()
	u := Usage{
		InputTokens:              200,
		OutputTokens:             150,
		CacheReadInputTokens:     40,
		CacheCreationInputTokens: 5,
		OutputTokensDetails:      OutputTokensDetails{ThinkingTokens: 90},
	}

	got := betaUsageFromMessage(u)

	require.NotNil(t, got)
	assert.Equal(t, int64(200), got.InputTokens)
	assert.Equal(t, int64(150), got.OutputTokens)
	assert.Equal(t, int64(40), got.CachedInputTokens)
	assert.Equal(t, int64(5), got.CacheWriteTokens)
	assert.Equal(t, int64(90), got.ReasoningTokens,
		"thinking tokens must be surfaced as ReasoningTokens on the Beta path too")
}

func TestFinishReason(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		stopReason StopReason
		sawToolUse bool
		want       chat.FinishReason
	}{
		{"end_turn", StopReasonEndTurn, false, chat.FinishReasonStop},
		{"stop_sequence", StopReasonStopSequence, false, chat.FinishReasonStop},
		{"tool_use", StopReasonToolUse, true, chat.FinishReasonToolCalls},
		{"max_tokens", StopReasonMaxTokens, false, chat.FinishReasonLength},
		{"max_tokens with tool use seen", StopReasonMaxTokens, true, chat.FinishReasonLength},
		{"model_context_window_exceeded", StopReasonModelContextWindowExceeded, false, chat.FinishReasonLength},
		{"model_context_window_exceeded with tool use seen", StopReasonModelContextWindowExceeded, true, chat.FinishReasonLength},
		{"refusal", StopReasonRefusal, false, chat.FinishReasonRefusal},
		{"refusal with tool use seen", StopReasonRefusal, true, chat.FinishReasonRefusal},
		{"missing stop reason without tool use", "", false, chat.FinishReasonStop},
		{"missing stop reason with tool use", "", true, chat.FinishReasonToolCalls},
		{"unknown stop reason falls back to tool use flag", "pause_turn", true, chat.FinishReasonToolCalls},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, finishReason(tt.stopReason, tt.sawToolUse))
		})
	}
}
