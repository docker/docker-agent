package modelinfo

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/effort"
)

func TestLookupClaude5(t *testing.T) {
	t.Parallel()

	// Expected values are written out from the #4583 matrix, not read from the table.
	known := []struct {
		id   string
		want Claude5Model
	}{
		{"claude-haiku-5-5", Claude5Model{CanDisableThinking: true, ChecksThinkingPrefix: true, NoServerSideFallback: true}},
		{"claude-opus-5", Claude5Model{CanDisableThinking: true}},
		{"claude-opus-5-5", Claude5Model{ChecksThinkingPrefix: true}},
		{"claude-sonnet-5", Claude5Model{CanDisableThinking: true}},
		{"claude-sonnet-5-5", Claude5Model{ChecksThinkingPrefix: true}},
		{"claude-fable-5", Claude5Model{}},
		{"claude-fable-5-1", Claude5Model{ChecksThinkingPrefix: true}},
		{"claude-mythos-5", Claude5Model{BedrockMantleOnly: true}},
		{"claude-mythos-5-1", Claude5Model{}},

		{"anthropic.claude-opus-5-5", Claude5Model{ChecksThinkingPrefix: true}},
		{"us.anthropic.claude-sonnet-5-5-v1:0", Claude5Model{ChecksThinkingPrefix: true}},
		{"global.anthropic.claude-fable-5-1", Claude5Model{ChecksThinkingPrefix: true}},
		{"claude-opus-5-5@20261001", Claude5Model{ChecksThinkingPrefix: true}},
		{"vendor/claude-fable-5-1", Claude5Model{ChecksThinkingPrefix: true}},
		{"CLAUDE-HAIKU-5-5", Claude5Model{CanDisableThinking: true, ChecksThinkingPrefix: true, NoServerSideFallback: true}},
	}
	for _, tc := range known {
		t.Run(tc.id, func(t *testing.T) {
			t.Parallel()
			got, ok := LookupClaude5(tc.id)
			assert.True(t, ok)
			assert.Equal(t, tc.want, got)
		})
	}

	for _, id := range []string{
		"openai/claude-opus-5-5",
		"claude-haiku-5",
		"claude-haiku-4-5",
		"claude-mythos-5-preview",
		"claude-opus-5-6",
		"claude-opus-5-5-20261001",
		"gpt-5",
		"",
	} {
		t.Run("unknown "+id, func(t *testing.T) {
			t.Parallel()
			_, ok := LookupClaude5(id)
			assert.False(t, ok)
		})
	}
}

func TestClaude5Capabilities(t *testing.T) {
	t.Parallel()

	ids := []string{
		"claude-haiku-5-5", "claude-opus-5", "claude-opus-5-5", "claude-sonnet-5", "claude-sonnet-5-5",
		"claude-fable-5", "claude-fable-5-1", "claude-mythos-5", "claude-mythos-5-1",
		"us.anthropic.claude-haiku-5-5",
	}
	for _, id := range ids {
		t.Run(id, func(t *testing.T) {
			t.Parallel()
			assert.True(t, RejectsTokenThinking(id), "RejectsTokenThinking")
			assert.True(t, SupportsAdaptiveThinking(id), "SupportsAdaptiveThinking")
			assert.False(t, SupportsFullThinkingDisplay(id), "SupportsFullThinkingDisplay")
			assert.Equal(t, []effort.Level{effort.XHigh, effort.Max}, anthropicTopEfforts(id))
		})
	}

	for _, provider := range []string{"anthropic", "amazon-bedrock"} {
		assert.Equal(t,
			[]effort.Level{effort.None, effort.Low, effort.Medium, effort.High, effort.XHigh, effort.Max},
			SupportedThinkingLevels(provider, "claude-sonnet-5-5"), provider)
	}
}
