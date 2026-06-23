package anthropic

import (
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
)

// TestUsageFromDelta_RecordsReasoningTokens pins the fix for the invisible
// Anthropic thinking-cost bug: extended-thinking tokens are billed inside
// OutputTokens but were never surfaced as ReasoningTokens, so the cost dialog
// and usage dashboards showed zero reasoning for Claude while every other
// provider (OpenAI, Gemini) reported it. The SDK exposes the breakdown via
// OutputTokensDetails.ThinkingTokens; this asserts we map it through.
func TestUsageFromDelta_RecordsReasoningTokens(t *testing.T) {
	u := anthropic.MessageDeltaUsage{
		InputTokens:              100,
		OutputTokens:             80,
		CacheReadInputTokens:     20,
		CacheCreationInputTokens: 10,
		OutputTokensDetails:      anthropic.OutputTokensDetails{ThinkingTokens: 55},
	}

	got := usageFromDelta(u)

	require.NotNil(t, got)
	assert.Equal(t, int64(100), got.InputTokens)
	assert.Equal(t, int64(80), got.OutputTokens)
	assert.Equal(t, int64(20), got.CachedInputTokens)
	assert.Equal(t, int64(10), got.CacheWriteTokens)
	assert.Equal(t, int64(55), got.ReasoningTokens,
		"thinking tokens must be surfaced as ReasoningTokens, not dropped")
}

// TestBetaUsageFromDelta_RecordsReasoningTokens is the Beta-API twin of the
// above. Interleaved thinking (the common case for extended-thinking agents)
// routes through the Beta stream, so the breakdown must be mapped there too.
func TestBetaUsageFromDelta_RecordsReasoningTokens(t *testing.T) {
	u := anthropic.BetaMessageDeltaUsage{
		InputTokens:              200,
		OutputTokens:             150,
		CacheReadInputTokens:     40,
		CacheCreationInputTokens: 5,
		OutputTokensDetails:      anthropic.BetaOutputTokensDetails{ThinkingTokens: 90},
	}

	got := betaUsageFromDelta(u)

	require.NotNil(t, got)
	assert.Equal(t, int64(200), got.InputTokens)
	assert.Equal(t, int64(150), got.OutputTokens)
	assert.Equal(t, int64(40), got.CachedInputTokens)
	assert.Equal(t, int64(5), got.CacheWriteTokens)
	assert.Equal(t, int64(90), got.ReasoningTokens,
		"thinking tokens must be surfaced as ReasoningTokens on the Beta path too")
}

func TestFinishReason(t *testing.T) {
	tests := []struct {
		name       string
		stopReason anthropic.StopReason
		sawToolUse bool
		want       chat.FinishReason
	}{
		{"end_turn", anthropic.StopReasonEndTurn, false, chat.FinishReasonStop},
		{"stop_sequence", anthropic.StopReasonStopSequence, false, chat.FinishReasonStop},
		{"tool_use", anthropic.StopReasonToolUse, true, chat.FinishReasonToolCalls},
		{"max_tokens", anthropic.StopReasonMaxTokens, false, chat.FinishReasonLength},
		{"refusal", anthropic.StopReasonRefusal, false, chat.FinishReasonRefusal},
		{"refusal with tool use seen", anthropic.StopReasonRefusal, true, chat.FinishReasonRefusal},
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
