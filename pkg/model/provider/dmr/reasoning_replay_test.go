package dmr

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/tools"
)

// Chat Completions reasoning replay defaults on and
// provider_opts.replay_reasoning: false turns it off.
func TestConvertMessages_ReasoningReplay(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		opts     map[string]any
		wantSent bool
	}{
		{name: "on by default", wantSent: true},
		{name: "explicit opt-out", opts: map[string]any{"replay_reasoning": false}, wantSent: false},
		{name: "explicit true keeps replay enabled", opts: map[string]any{"replay_reasoning": true}, wantSent: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := &Client{Config: base.Config{ModelConfig: latest.ModelConfig{
				Provider:     "dmr",
				Model:        "ai/qwen3",
				ProviderOpts: tt.opts,
			}}}

			converted := c.convertMessages(t.Context(), []chat.Message{
				{Role: chat.MessageRoleUser, Content: "weather?"},
				{
					Role:             chat.MessageRoleAssistant,
					Content:          "checking",
					ReasoningContent: "I should call the weather tool",
					ToolCalls:        []tools.ToolCall{{ID: "c1", Function: tools.FunctionCall{Name: "weather", Arguments: `{}`}}},
				},
			})

			raw, err := json.Marshal(converted)
			require.NoError(t, err)

			var msgs []map[string]any
			require.NoError(t, json.Unmarshal(raw, &msgs))

			var assistant map[string]any
			for _, m := range msgs {
				if m["role"] == "assistant" {
					assistant = m
				}
			}
			require.NotNil(t, assistant, "assistant turn must survive conversion: %s", raw)

			if tt.wantSent {
				assert.Equal(t, "I should call the weather tool", assistant["reasoning_content"])
			} else {
				assert.NotContains(t, assistant, "reasoning_content")
			}
			assert.Len(t, assistant["tool_calls"], 1)
		})
	}
}
