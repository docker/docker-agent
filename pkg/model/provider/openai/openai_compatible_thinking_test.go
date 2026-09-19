package openai

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/tools"
)

// driveChatCompletions runs one Chat Completions request for cfg against a mock server and returns the request body.
func driveChatCompletions(t *testing.T, cfg *latest.ModelConfig, toolList []tools.Tool, opts ...options.Opt) map[string]any {
	t.Helper()

	server, body := captureRequestBody(t)
	cfg.BaseURL = server.URL
	cfg.TokenKey = "MY_TOKEN"
	env := environment.NewMapEnvProvider(map[string]string{"MY_TOKEN": "secret"})

	client, err := NewClient(t.Context(), cfg, env, opts...)
	require.NoError(t, err)

	stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hi"}}, toolList)
	require.NoError(t, err)
	defer stream.Close()
	drainReasoningTestStream(t, stream)

	var req map[string]any
	require.NoError(t, json.Unmarshal(body(), &req))
	return req
}

func TestChatCompletions_ChatTemplateThinkingOff(t *testing.T) {
	t.Parallel()

	for _, budget := range []*latest.ThinkingBudget{{Effort: "none"}, {Tokens: 0}} {
		req := driveChatCompletions(t, &latest.ModelConfig{
			Provider:       "openai",
			Model:          "mlx-community/Qwen3.6-35B-A3B-8bit",
			ThinkingBudget: budget,
		}, nil, options.WithChatTemplateThinkingOff(true))

		assert.Equal(t, map[string]any{"enable_thinking": false}, req["chat_template_kwargs"])
		assert.NotContains(t, req, "reasoning_effort")
		assert.NotContains(t, req, "max_tokens", "no cap configured, so nothing to floor")
	}
}

func TestChatCompletions_ChatTemplateThinkingOff_FloorsMaxTokens(t *testing.T) {
	t.Parallel()

	maxTokens := int64(20)
	req := driveChatCompletions(t, &latest.ModelConfig{
		Provider:       "openai",
		Model:          "qwen3",
		MaxTokens:      &maxTokens,
		ThinkingBudget: &latest.ThinkingBudget{Effort: "none"},
	}, nil, options.WithChatTemplateThinkingOff(true))

	assert.Equal(t, map[string]any{"enable_thinking": false}, req["chat_template_kwargs"])
	assert.EqualValues(t, noThinkingMinOutputTokens, req["max_tokens"])
}

// The title-generation and compaction shape: NoThinking plus the clone's injected none budget.
func TestChatCompletions_ChatTemplateThinkingOff_NoThinkingClone(t *testing.T) {
	t.Parallel()

	req := driveChatCompletions(t, &latest.ModelConfig{
		Provider:       "openai",
		Model:          "qwen3",
		ThinkingBudget: &latest.ThinkingBudget{Effort: "none"},
	}, nil, options.WithChatTemplateThinkingOff(true), options.WithNoThinking())

	assert.Equal(t, map[string]any{"enable_thinking": false}, req["chat_template_kwargs"])
	assert.NotContains(t, req, "reasoning_effort")
}

func TestChatCompletions_ChatTemplateThinkingOff_NotSentWithoutTheOption(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  *latest.ModelConfig
		opts []options.Opt
	}{
		{
			name: "thinking_budget: none without the factory bit",
			cfg:  &latest.ModelConfig{Provider: "openai", Model: "qwen3", ThinkingBudget: &latest.ThinkingBudget{Effort: "none"}},
		},
		{
			name: "NoThinking clone without the factory bit",
			cfg:  &latest.ModelConfig{Provider: "openai", Model: "qwen3", ThinkingBudget: &latest.ThinkingBudget{Effort: "none"}},
			opts: []options.Opt{options.WithNoThinking()},
		},
		{
			name: "thinking_budget unset",
			cfg:  &latest.ModelConfig{Provider: "openai", Model: "qwen3"},
		},
		{
			name: "thinking_budget enabled",
			cfg:  &latest.ModelConfig{Provider: "openai", Model: "qwen3", ThinkingBudget: &latest.ThinkingBudget{Effort: "high"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := driveChatCompletions(t, tt.cfg, nil, tt.opts...)
			assert.NotContains(t, req, "chat_template_kwargs")
			assert.NotContains(t, req, "reasoning_effort")
		})
	}
}

func TestChatCompletions_ExtraBody_ForwardedOnAnyProvider(t *testing.T) {
	t.Parallel()

	req := driveChatCompletions(t, &latest.ModelConfig{
		Provider: "groq",
		Model:    "qwen/qwen3-32b",
		ProviderOpts: map[string]any{
			"top_k": 40,
			"extra_body": map[string]any{
				"reasoning_effort": "none",
				"thinking":         map[string]any{"type": "disabled"},
			},
		},
	}, nil)

	assert.Equal(t, "none", req["reasoning_effort"])
	assert.Equal(t, map[string]any{"type": "disabled"}, req["thinking"])
	assert.EqualValues(t, 40, req["top_k"], "sampling opts still travel alongside extra_body")
}

func TestChatCompletions_ExtraBody_OverridesDerivedFields(t *testing.T) {
	t.Parallel()

	req := driveChatCompletions(t, &latest.ModelConfig{
		Provider:       "openai",
		Model:          "qwen3",
		ThinkingBudget: &latest.ThinkingBudget{Effort: "none"},
		ProviderOpts: map[string]any{
			"extra_body": map[string]any{
				"chat_template_kwargs": map[string]any{"enable_thinking": true},
			},
		},
	}, nil, options.WithChatTemplateThinkingOff(true))

	assert.Equal(t, map[string]any{"enable_thinking": true}, req["chat_template_kwargs"], "explicit extra_body wins over the thinking_budget switch")
}
