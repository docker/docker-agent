package compaction

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/tools"
)

// Scenario: reasoning tokens exceeding output tokens must not produce a
// negative "reported" size; the estimator should fall back to the
// heuristic instead of returning a negative estimate.
func TestScenario_EstimateMessageTokens_ReasoningExceedsOutput(t *testing.T) {
	msg := &chat.Message{
		Role:    chat.MessageRoleAssistant,
		Content: strings.Repeat("x", 350), // ~100 heuristic tokens
		Usage: &chat.Usage{
			OutputTokens:    10,
			ReasoningTokens: 50, // provider glitch or pure-reasoning turn
		},
	}
	got := EstimateMessageTokens(msg)
	assert.Positive(t, got, "estimate must never be negative or zero")
	assert.Equal(t, int64(float64(350)/charsPerToken)+perMessageOverhead, got,
		"should fall back to the heuristic when reported <= 0")
}

// Scenario: an anchor whose prompt count is zero but output is non-zero is
// treated as in-between content; calibration must not divide by zero or
// produce a wild scale.
func TestScenario_NewEstimator_OutputOnlyUsage(t *testing.T) {
	msgs := []chat.Message{
		{Role: chat.MessageRoleUser, Content: strings.Repeat("u", 4000)},
		{Role: chat.MessageRoleAssistant, Content: "a", Usage: &chat.Usage{OutputTokens: 5}},
		{Role: chat.MessageRoleUser, Content: strings.Repeat("v", 4000)},
	}
	e := NewSliceEstimator(msgs)
	assert.InDelta(t, 1.0, e.Scale(), 0, "no usable anchors -> neutral estimator")
}

// Scenario: consecutive anchors with a compaction in between (prompt shrank)
// must be discarded, not fold a negative delta into the ratio.
func TestScenario_NewEstimator_CompactionBetweenAnchors(t *testing.T) {
	msgs := []chat.Message{
		{Role: chat.MessageRoleAssistant, Model: "m", Usage: &chat.Usage{InputTokens: 50_000, OutputTokens: 100}},
		{Role: chat.MessageRoleTool, Content: strings.Repeat("t", 4000)},
		// Compaction happened: prompt dropped from 50k to 2k.
		{Role: chat.MessageRoleAssistant, Model: "m", Usage: &chat.Usage{InputTokens: 2_000, OutputTokens: 100}},
	}
	e := NewSliceEstimator(msgs)
	assert.InDelta(t, 1.0, e.Scale(), 0, "negative window delta must be discarded")
}

// Scenario: SplitIndexForKeep on a conversation that ends with a huge tool
// result larger than the keep budget. The kept tail should snap to a
// user/assistant boundary (never start on the tool message), or keep
// nothing at all.
func TestScenario_SplitIndexForKeep_HugeTrailingToolResult(t *testing.T) {
	msgs := []chat.Message{
		{Role: chat.MessageRoleUser, Content: "question"},
		{Role: chat.MessageRoleAssistant, Content: "calling tool", ToolCalls: []tools.ToolCall{{}}},
		{Role: chat.MessageRoleTool, Content: strings.Repeat("t", 400_000)}, // ~114k tokens
	}
	idx := SplitIndexForKeep(msgs, 20_000)
	if idx < len(msgs) {
		role := msgs[idx].Role
		assert.True(t, role == chat.MessageRoleUser || role == chat.MessageRoleAssistant,
			"kept tail must start on a user/assistant boundary, got %q", role)
	}
	// The tool result exceeds the budget on its own, so nothing can be
	// kept: idx == len(msgs) (compact everything).
	assert.Equal(t, len(msgs), idx)
}

// Scenario: a kept-tail boundary must never split an assistant tool-call
// message from its tool results.
func TestScenario_SplitIndexForKeep_NeverOrphansToolResults(t *testing.T) {
	big := strings.Repeat("x", 3500) // ~1000 tokens each
	var msgs []chat.Message
	for range 20 {
		msgs = append(msgs,
			chat.Message{Role: chat.MessageRoleUser, Content: big},
			chat.Message{Role: chat.MessageRoleAssistant, Content: big, ToolCalls: []tools.ToolCall{{ID: "c"}}},
			chat.Message{Role: chat.MessageRoleTool, ToolCallID: "c", Content: big},
			chat.Message{Role: chat.MessageRoleAssistant, Content: big},
		)
	}
	for budget := int64(500); budget <= 30_000; budget += 777 {
		idx := SplitIndexForKeep(msgs, budget)
		if idx < len(msgs) {
			assert.NotEqual(t, chat.MessageRoleTool, msgs[idx].Role,
				"budget %d: kept tail starts on a tool result", budget)
		}
	}
}

// Scenario: threshold values above 1 (user misconfiguration like 1.5 or 90
// meaning "90%") silently fall back to the default instead of erroring.
func TestScenario_ShouldCompact_ThresholdAboveOne(t *testing.T) {
	// 95k tokens of a 100k window, user config "compaction_threshold: 95"
	// (meant 0.95). Falls back to 0.9 -> compacts at 91k.
	assert.True(t, ShouldCompact(91_000, 0, 0, 100_000, 95))
	assert.False(t, ShouldCompact(89_000, 0, 0, 100_000, 95))
}
