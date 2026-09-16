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
)

// compatChatRequest drives one Chat Completions request for cfg against
// a mock server and returns the decoded request body. The server URL is
// written into cfg.BaseURL; whether that counts as a user-chosen endpoint is
// controlled by the caller through options.WithCustomBaseURL.
func compatChatRequest(t *testing.T, cfg *latest.ModelConfig, opts ...options.Opt) map[string]any {
	t.Helper()

	server, body := captureRequestBody(t)
	cfg.BaseURL = server.URL
	cfg.TokenKey = "MY_TOKEN"
	env := environment.NewMapEnvProvider(map[string]string{"MY_TOKEN": "secret"})

	client, err := NewClient(t.Context(), cfg, env, opts...)
	require.NoError(t, err)

	stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hi"}}, nil)
	require.NoError(t, err)
	defer stream.Close()
	drainReasoningTestStream(t, stream)

	var req map[string]any
	require.NoError(t, json.Unmarshal(body(), &req))
	return req
}

// optedIn mirrors what the factory threads to the client for a model on a
// user-chosen endpoint whose YAML disabled thinking.
var optedIn = []options.Opt{options.WithCustomBaseURL(true), options.WithThinkingDisabled(true)}

func TestChatCompletions_ThinkingDisabled_CustomBaseURL(t *testing.T) {
	t.Parallel()

	for _, budget := range []*latest.ThinkingBudget{{Effort: "none"}, {Tokens: 0}} {
		req := compatChatRequest(t, &latest.ModelConfig{
			Provider:       "openai",
			Model:          "mlx-community/Qwen3.6-35B-A3B-8bit",
			ThinkingBudget: budget,
		}, optedIn...)

		assert.Equal(t, map[string]any{"enable_thinking": false}, req["chat_template_kwargs"])
		assert.NotContains(t, req, "reasoning_effort")
		assert.NotContains(t, req, "max_tokens", "no cap configured, so nothing to floor")
	}
}

func TestChatCompletions_ThinkingDisabled_CustomBaseURL_FloorsMaxTokens(t *testing.T) {
	t.Parallel()

	maxTokens := int64(20)
	req := compatChatRequest(t, &latest.ModelConfig{
		Provider:       "openai",
		Model:          "qwen3",
		MaxTokens:      &maxTokens,
		ThinkingBudget: &latest.ThinkingBudget{Effort: "none"},
	}, optedIn...)

	assert.Equal(t, map[string]any{"enable_thinking": false}, req["chat_template_kwargs"])
	assert.EqualValues(t, noThinkingMinOutputTokens, req["max_tokens"])
}

// TestChatCompletions_ThinkingDisabled_NoThinkingClone is the title-generation
// and compaction shape: the clone carries NoThinking plus the injected none
// budget, and still sends the switch because the user opted in.
func TestChatCompletions_ThinkingDisabled_NoThinkingClone(t *testing.T) {
	t.Parallel()

	req := compatChatRequest(t, &latest.ModelConfig{
		Provider:       "openai",
		Model:          "qwen3",
		ThinkingBudget: &latest.ThinkingBudget{Effort: "none"},
	}, append(optedIn, options.WithNoThinking())...)

	assert.Equal(t, map[string]any{"enable_thinking": false}, req["chat_template_kwargs"])
	assert.NotContains(t, req, "reasoning_effort")
}

func TestChatCompletions_ThinkingSwitch_NotSentWithoutOptIn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  *latest.ModelConfig
		opts []options.Opt
	}{
		{
			name: "base_url is the alias default",
			cfg:  &latest.ModelConfig{Provider: "xai", Model: "grok-4", ThinkingBudget: &latest.ThinkingBudget{Effort: "none"}},
			opts: []options.Opt{options.WithThinkingDisabled(true)},
		},
		{
			name: "OpenAI model name behind a proxy",
			cfg:  &latest.ModelConfig{Provider: "openai", Model: "gpt-4o", ThinkingBudget: &latest.ThinkingBudget{Effort: "none"}},
			opts: optedIn,
		},
		{
			name: "azure deployment with an arbitrary name",
			cfg:  &latest.ModelConfig{Provider: "azure", Model: "my-qwen-deployment", ThinkingBudget: &latest.ThinkingBudget{Effort: "none"}, ProviderOpts: map[string]any{"api_type": "openai_chatcompletions"}},
			opts: optedIn,
		},
		{
			name: "internal NoThinking clone of a model whose YAML never disabled thinking",
			cfg:  &latest.ModelConfig{Provider: "openai", Model: "qwen3", ThinkingBudget: &latest.ThinkingBudget{Effort: "none"}},
			opts: []options.Opt{options.WithCustomBaseURL(true), options.WithNoThinking()},
		},
		{
			name: "thinking_budget unset",
			cfg:  &latest.ModelConfig{Provider: "openai", Model: "qwen3"},
			opts: []options.Opt{options.WithCustomBaseURL(true)},
		},
		{
			name: "thinking_budget enabled",
			cfg:  &latest.ModelConfig{Provider: "openai", Model: "qwen3", ThinkingBudget: &latest.ThinkingBudget{Effort: "high"}},
			opts: []options.Opt{options.WithCustomBaseURL(true)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := compatChatRequest(t, tt.cfg, tt.opts...)
			assert.NotContains(t, req, "chat_template_kwargs")
			assert.NotContains(t, req, "reasoning_effort")
		})
	}
}

func TestChatCompletions_ExtraBody_ForwardedOnAnyProvider(t *testing.T) {
	t.Parallel()

	req := compatChatRequest(t, &latest.ModelConfig{
		Provider: "groq",
		Model:    "qwen/qwen3-32b",
		ProviderOpts: map[string]any{
			"top_k": 40,
			"extra_body": map[string]any{
				"reasoning_effort": "none",
				"thinking":         map[string]any{"type": "disabled"},
			},
		},
	})

	assert.Equal(t, "none", req["reasoning_effort"])
	assert.Equal(t, map[string]any{"type": "disabled"}, req["thinking"])
	assert.EqualValues(t, 40, req["top_k"], "sampling opts still travel alongside extra_body")
}

func TestChatCompletions_ExtraBody_OverridesDerivedFields(t *testing.T) {
	t.Parallel()

	req := compatChatRequest(t, &latest.ModelConfig{
		Provider:       "openai",
		Model:          "qwen3",
		ThinkingBudget: &latest.ThinkingBudget{Effort: "none"},
		ProviderOpts: map[string]any{
			"extra_body": map[string]any{
				"chat_template_kwargs": map[string]any{"enable_thinking": true},
			},
		},
	}, optedIn...)

	assert.Equal(t, map[string]any{"enable_thinking": true}, req["chat_template_kwargs"], "explicit extra_body wins over the thinking_budget sugar")
}
