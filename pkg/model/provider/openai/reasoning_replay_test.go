package openai

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

// assistantWire marshals the converted request and returns the first assistant
// message as a raw map, so the test asserts on the wire shape the endpoint
// actually receives -- reasoning_content is a non-standard extension that only
// survives marshalling, never the typed param.
func assistantWire(t *testing.T, c *Client, messages []chat.Message) map[string]any {
	t.Helper()

	raw, err := json.Marshal(c.convertMessages(t.Context(), messages))
	require.NoError(t, err)

	var msgs []map[string]any
	require.NoError(t, json.Unmarshal(raw, &msgs))
	for _, m := range msgs {
		if m["role"] == "assistant" {
			return m
		}
	}
	t.Fatalf("no assistant message in request: %s", raw)
	return nil
}

func reasoningHistory() []chat.Message {
	return []chat.Message{
		{Role: chat.MessageRoleUser, Content: "weather?"},
		{
			Role:             chat.MessageRoleAssistant,
			Content:          "checking",
			ReasoningContent: "I should call the weather tool",
			ToolCalls:        []tools.ToolCall{{ID: "c1", Function: tools.FunctionCall{Name: "weather", Arguments: `{}`}}},
		},
	}
}

// Reasoning replay is a Chat Completions capability, not an endpoint
// classification. Every destination defaults on and may opt out explicitly.
func TestConvertMessages_ReasoningReplayDefaultsOn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		provider string
		baseURL  string
		opts     map[string]any
	}{
		{name: "custom provider", provider: "local_llm", baseURL: "http://127.0.0.1:8080/v1", opts: map[string]any{"api_type": "openai_chatcompletions"}},
		{name: "first-party OpenAI", provider: "openai"},
		{name: "Azure", provider: "azure", baseURL: "https://team.openai.azure.com/openai/deployments/gpt-5.6"},
		{name: "hosted alias", provider: "xai"},
		{name: "DeepSeek", provider: "deepseek"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := &Client{Config: base.Config{ModelConfig: latest.ModelConfig{
				Provider: tt.provider, Model: "qwen3", BaseURL: tt.baseURL, ProviderOpts: tt.opts,
			}}}
			msg := assistantWire(t, c, reasoningHistory())
			assert.Equal(t, "I should call the weather tool", msg["reasoning_content"])
			assert.Len(t, msg["tool_calls"], 1)
		})
	}
}

func TestConvertMessages_ReasoningReplayOverride(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		provider string
		baseURL  string
		opts     map[string]any
		wantSent bool
	}{
		{
			name:     "explicit true keeps replay enabled on OpenAI",
			provider: "openai",
			opts:     map[string]any{"replay_reasoning": true},
			wantSent: true,
		},
		{
			name:     "explicit true keeps replay enabled on a hosted alias",
			provider: "xai",
			opts:     map[string]any{"replay_reasoning": true},
			wantSent: true,
		},
		{
			name:     "custom provider opts out",
			provider: "openai",
			baseURL:  "http://127.0.0.1:8080/v1",
			opts:     map[string]any{"api_type": "openai_chatcompletions", "replay_reasoning": false},
			wantSent: false,
		},
		{
			name:     "explicit true keeps replay enabled on ChatGPT",
			provider: "chatgpt",
			opts:     map[string]any{"replay_reasoning": true},
			wantSent: true,
		},
		{
			// A value that is present but not a boolean must not inherit the
			// on default: the option is how an operator keeps reasoning away
			// from an endpoint, so a quoted "false" has to mean off.
			name:     "non-boolean value fails closed on a custom provider",
			provider: "local_llm",
			baseURL:  "http://127.0.0.1:8080/v1",
			opts:     map[string]any{"api_type": "openai_chatcompletions", "replay_reasoning": "false"},
		},
		{
			name:     "non-boolean value fails closed on a hosted alias",
			provider: "xai",
			opts:     map[string]any{"replay_reasoning": "true"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := &Client{Config: base.Config{ModelConfig: latest.ModelConfig{
				Provider:     tt.provider,
				Model:        "qwen3",
				BaseURL:      tt.baseURL,
				ProviderOpts: tt.opts,
			}}}

			msg := assistantWire(t, c, reasoningHistory())
			if tt.wantSent {
				assert.Equal(t, "I should call the weather tool", msg["reasoning_content"])
			} else {
				assert.NotContains(t, msg, "reasoning_content")
			}
		})
	}
}

// Replay also applies before the prompt-cache breakpoint path is chosen, so the
// body the cache key is computed from is the body that gets sent.
func TestConvertMessages_ReasoningReplayAppliesToBreakpointPath(t *testing.T) {
	t.Parallel()

	c := &Client{Config: base.Config{ModelConfig: latest.ModelConfig{
		Provider: "openai",
		Model:    "gpt-5.6",
	}}}
	require.True(t, sendsExplicitCacheBreakpoints(&c.ModelConfig, false),
		"test is only meaningful on the explicit-breakpoint path")

	msg := assistantWire(t, c, reasoningHistory())
	assert.Equal(t, "I should call the weather tool", msg["reasoning_content"])
}
