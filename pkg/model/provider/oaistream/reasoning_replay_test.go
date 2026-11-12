package oaistream

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestReplayReasoning(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts map[string]any
		want bool
	}{
		{name: "defaults on", want: true},
		{
			name: "explicit true",
			opts: map[string]any{ProviderOptReplayReasoning: true},
			want: true,
		},
		{
			name: "explicit false",
			opts: map[string]any{ProviderOptReplayReasoning: false},
			want: false,
		},
		{
			// A quoted value is a typo, and the option exists to keep reasoning
			// away from an endpoint, so it must fail closed.
			name: "non-bool value fails closed",
			opts: map[string]any{ProviderOptReplayReasoning: "yes"},
			want: false,
		},
		{
			name: "unrelated provider opts ignored",
			opts: map[string]any{"context_size": float64(8192)},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, ReplayReasoning(tt.opts))
		})
	}
}

func TestWithoutReplayedReasoning(t *testing.T) {
	t.Parallel()

	in := []chat.Message{
		{Role: chat.MessageRoleSystem, Content: "be helpful"},
		{Role: chat.MessageRoleUser, Content: "weather?"},
		{
			Role:             chat.MessageRoleAssistant,
			ReasoningContent: "call the weather tool",
			ToolCalls:        []tools.ToolCall{{ID: "c1", Function: tools.FunctionCall{Name: "weather", Arguments: `{}`}}},
		},
		{Role: chat.MessageRoleTool, ToolCallID: "c1", Content: "sunny"},
		{Role: chat.MessageRoleAssistant, Content: "it is sunny", ReasoningContent: "answer"},
	}
	// Snapshot so the no-mutation contract is checked against the original.
	original := slices.Clone(in)

	got := WithoutReplayedReasoning(in)

	assert.Equal(t, "be helpful", got[0].Content)
	assert.Equal(t, "weather?", got[1].Content)

	// Reasoning is gone, but the turn itself stays convertible.
	assert.Empty(t, got[2].ReasoningContent)
	assert.Len(t, got[2].ToolCalls, 1)
	assert.Equal(t, "c1", got[2].ToolCalls[0].ID)

	assert.Equal(t, "sunny", got[3].Content)
	assert.Empty(t, got[4].ReasoningContent)
	assert.Equal(t, "it is sunny", got[4].Content)

	assert.Equal(t, original, in, "input messages must not be mutated")
}

func TestWithoutReplayedReasoning_NothingToStrip(t *testing.T) {
	t.Parallel()

	in := []chat.Message{
		{Role: chat.MessageRoleUser, Content: "hi"},
		{Role: chat.MessageRoleAssistant, Content: "hello"},
		// Reasoning on a non-assistant role is never converted, so it must not
		// force a copy of the whole history.
		{Role: chat.MessageRoleUser, Content: "again", ReasoningContent: "stray"},
	}

	got := WithoutReplayedReasoning(in)

	assert.Equal(t, in, got)
	assert.Equal(t, "stray", got[2].ReasoningContent)
}
