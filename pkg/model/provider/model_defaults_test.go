package provider

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
)

func TestApplyModelDefaults(t *testing.T) {
	t.Parallel()

	boolPtr := func(v bool) *bool { return &v }

	tests := []struct {
		name            string
		config          *latest.ModelConfig
		wantBudget      *latest.ThinkingBudget // nil means no thinking
		wantInterleaved *bool                  // nil means key must not exist
	}{
		// --- OpenAI: only o-series gets defaults ---
		{
			name:   "openai/gpt-4o: no default thinking",
			config: &latest.ModelConfig{Provider: "openai", Model: "gpt-4o"},
		},
		{
			name:   "openai/gpt-5: no default thinking",
			config: &latest.ModelConfig{Provider: "openai", Model: "gpt-5"},
		},
		{
			name:       "openai/o3-mini: thinking-only model gets default",
			config:     &latest.ModelConfig{Provider: "openai", Model: "o3-mini"},
			wantBudget: &latest.ThinkingBudget{Effort: "medium"},
		},
		{
			name:       "openai/o1: thinking-only model gets default",
			config:     &latest.ModelConfig{Provider: "openai", Model: "o1"},
			wantBudget: &latest.ThinkingBudget{Effort: "medium"},
		},
		{
			name:       "openai/o4-mini: thinking-only model gets default",
			config:     &latest.ModelConfig{Provider: "openai", Model: "o4-mini"},
			wantBudget: &latest.ThinkingBudget{Effort: "medium"},
		},
		{
			name:       "openai/o3-mini: explicit budget overrides default",
			config:     &latest.ModelConfig{Provider: "openai", Model: "o3-mini", ThinkingBudget: &latest.ThinkingBudget{Effort: "high"}},
			wantBudget: &latest.ThinkingBudget{Effort: "high"},
		},
		{
			name:       "openai/gpt-4o: explicit budget preserved",
			config:     &latest.ModelConfig{Provider: "openai", Model: "gpt-4o", ThinkingBudget: &latest.ThinkingBudget{Effort: "high"}},
			wantBudget: &latest.ThinkingBudget{Effort: "high"},
		},

		// --- Aliases (resolve to openai) — no default thinking ---
		{
			name:   "mistral: no default thinking",
			config: &latest.ModelConfig{Provider: "mistral", Model: "mistral-large-latest"},
		},
		{
			name:   "xai: no default thinking",
			config: &latest.ModelConfig{Provider: "xai", Model: "grok-2"},
		},
		{
			name:   "xai: unrelated alias never preserves none, even with a gpt-5.6-shaped model name",
			config: &latest.ModelConfig{Provider: "xai", Model: "gpt-5.6", ThinkingBudget: &latest.ThinkingBudget{Effort: "none"}},
		},
		{
			name:   "mistral: unrelated alias never preserves none, even with a gpt-5.6-shaped model name",
			config: &latest.ModelConfig{Provider: "mistral", Model: "gpt-5.6-sol", ThinkingBudget: &latest.ThinkingBudget{Effort: "none"}},
		},
		{
			name:       "azure: genuine OpenAI vendor preserves none on gpt-5.6",
			config:     &latest.ModelConfig{Provider: "azure", Model: "gpt-5.6", ThinkingBudget: &latest.ThinkingBudget{Effort: "none"}},
			wantBudget: &latest.ThinkingBudget{Effort: "none"},
		},
		{
			name:       "chatgpt: genuine OpenAI vendor preserves none on gpt-5.6",
			config:     &latest.ModelConfig{Provider: "chatgpt", Model: "gpt-5.6", ThinkingBudget: &latest.ThinkingBudget{Effort: "none"}},
			wantBudget: &latest.ThinkingBudget{Effort: "none"},
		},
		{
			name:       "vercel with explicit openai/ qualified model preserves none",
			config:     &latest.ModelConfig{Provider: "vercel", Model: "openai/gpt-5.6-sol", ThinkingBudget: &latest.ThinkingBudget{Effort: "none"}},
			wantBudget: &latest.ThinkingBudget{Effort: "none"},
		},
		{
			name:   "vercel without an openai/ qualifier does not preserve none",
			config: &latest.ModelConfig{Provider: "vercel", Model: "gpt-5.6", ThinkingBudget: &latest.ThinkingBudget{Effort: "none"}},
		},
		{
			name:   "custom openai_chatcompletions: no default thinking",
			config: &latest.ModelConfig{Provider: "custom", Model: "custom-model", ProviderOpts: map[string]any{"api_type": "openai_chatcompletions"}},
		},

		// --- Anthropic: no default, but interleaved_thinking when budget set ---
		{
			name:   "anthropic: no default thinking",
			config: &latest.ModelConfig{Provider: "anthropic", Model: "claude-sonnet-4-0"},
		},
		{
			name:            "anthropic: explicit budget enables interleaved_thinking",
			config:          &latest.ModelConfig{Provider: "anthropic", Model: "claude-sonnet-4-0", ThinkingBudget: &latest.ThinkingBudget{Tokens: 16384}},
			wantBudget:      &latest.ThinkingBudget{Tokens: 16384},
			wantInterleaved: boolPtr(true),
		},
		{
			name:            "anthropic: adaptive budget enables interleaved_thinking",
			config:          &latest.ModelConfig{Provider: "anthropic", Model: "claude-opus-4-6", ThinkingBudget: &latest.ThinkingBudget{Effort: "adaptive"}},
			wantBudget:      &latest.ThinkingBudget{Effort: "adaptive"},
			wantInterleaved: boolPtr(true),
		},
		{
			name:            "anthropic: explicit interleaved_thinking=false is preserved",
			config:          &latest.ModelConfig{Provider: "anthropic", Model: "claude-sonnet-4-0", ThinkingBudget: &latest.ThinkingBudget{Tokens: 8192}, ProviderOpts: map[string]any{"interleaved_thinking": false}},
			wantBudget:      &latest.ThinkingBudget{Tokens: 8192},
			wantInterleaved: boolPtr(false),
		},

		// --- Google: no default thinking ---
		{
			name:   "google/gemini-2.5-flash: no default thinking",
			config: &latest.ModelConfig{Provider: "google", Model: "gemini-2.5-flash"},
		},
		{
			name:   "google/gemini-3-pro: no default thinking",
			config: &latest.ModelConfig{Provider: "google", Model: "gemini-3-pro"},
		},
		{
			name:       "google: explicit budget preserved",
			config:     &latest.ModelConfig{Provider: "google", Model: "gemini-2.5-flash", ThinkingBudget: &latest.ThinkingBudget{Tokens: 8192}},
			wantBudget: &latest.ThinkingBudget{Tokens: 8192},
		},

		// --- Bedrock: no default thinking, interleaved_thinking when budget set on Claude ---
		{
			name:   "bedrock claude: no default thinking",
			config: &latest.ModelConfig{Provider: "amazon-bedrock", Model: "anthropic.claude-3-sonnet"},
		},
		{
			name:   "bedrock global claude: no default thinking",
			config: &latest.ModelConfig{Provider: "amazon-bedrock", Model: "global.anthropic.claude-sonnet-4-5-20250929-v1:0"},
		},
		{
			name:            "bedrock claude: explicit budget enables interleaved_thinking",
			config:          &latest.ModelConfig{Provider: "amazon-bedrock", Model: "anthropic.claude-3-sonnet", ThinkingBudget: &latest.ThinkingBudget{Tokens: 8192}},
			wantBudget:      &latest.ThinkingBudget{Tokens: 8192},
			wantInterleaved: boolPtr(true),
		},
		{
			name:   "bedrock non-claude: not affected",
			config: &latest.ModelConfig{Provider: "amazon-bedrock", Model: "amazon.titan-text-express-v1"},
		},

		// --- Disabled thinking normalised to nil ---
		{
			name:   "thinking_budget: 0 becomes nil",
			config: &latest.ModelConfig{Provider: "anthropic", Model: "claude-sonnet-4-0", ThinkingBudget: &latest.ThinkingBudget{Tokens: 0}},
		},
		{
			name:   "thinking_budget: none becomes nil",
			config: &latest.ModelConfig{Provider: "openai", Model: "gpt-4o", ThinkingBudget: &latest.ThinkingBudget{Effort: "none"}},
		},
		{
			name:   "gpt-5.2: thinking_budget: none becomes nil (no real none effort)",
			config: &latest.ModelConfig{Provider: "openai", Model: "gpt-5.2", ThinkingBudget: &latest.ThinkingBudget{Effort: "none"}},
		},
		{
			name:   "anthropic: thinking_budget: none stays nil even on a model with a none-capable OpenAI sibling name",
			config: &latest.ModelConfig{Provider: "anthropic", Model: "gpt-5.6", ThinkingBudget: &latest.ThinkingBudget{Effort: "none"}},
		},
		{
			name:       "gpt-5.6: thinking_budget: none is preserved (real API none effort)",
			config:     &latest.ModelConfig{Provider: "openai", Model: "gpt-5.6", ThinkingBudget: &latest.ThinkingBudget{Effort: "none"}},
			wantBudget: &latest.ThinkingBudget{Effort: "none"},
		},
		{
			name:       "gpt-5.6-sol: thinking_budget: NONE (case-insensitive) is preserved",
			config:     &latest.ModelConfig{Provider: "openai", Model: "gpt-5.6-sol", ThinkingBudget: &latest.ThinkingBudget{Effort: "NONE"}},
			wantBudget: &latest.ThinkingBudget{Effort: "NONE"},
		},
		{
			name:       "gpt-5.6 via openai_responses api_type: thinking_budget: none is preserved",
			config:     &latest.ModelConfig{Provider: "custom", Model: "gpt-5.6-terra", ProviderOpts: map[string]any{"api_type": "openai_responses"}, ThinkingBudget: &latest.ThinkingBudget{Effort: "none"}},
			wantBudget: &latest.ThinkingBudget{Effort: "none"},
		},
		{
			name:   "gpt-5.6: thinking_budget: 0 (tokens, not effort) still becomes nil",
			config: &latest.ModelConfig{Provider: "openai", Model: "gpt-5.6", ThinkingBudget: &latest.ThinkingBudget{Tokens: 0}},
		},

		// --- Unknown / other providers: no effect ---
		{
			name:   "unknown provider: no effect",
			config: &latest.ModelConfig{Provider: "unknown", Model: "some-model"},
		},
		{
			name:   "dmr: no effect",
			config: &latest.ModelConfig{Provider: "dmr", Model: "ai/llama3.2"},
		},

		// --- Disabled thinking preserved where a client acts on it ---
		{
			name:       "dmr: thinking_budget: none is preserved (reasoning-budget 0 and chat_template_kwargs)",
			config:     &latest.ModelConfig{Provider: "dmr", Model: "ai/qwen3", ThinkingBudget: &latest.ThinkingBudget{Effort: "none"}},
			wantBudget: &latest.ThinkingBudget{Effort: "none"},
		},
		{
			name:       "dmr: thinking_budget: 0 is preserved",
			config:     &latest.ModelConfig{Provider: "dmr", Model: "ai/qwen3", ThinkingBudget: &latest.ThinkingBudget{Tokens: 0}},
			wantBudget: &latest.ThinkingBudget{Tokens: 0},
		},
		{
			name:       "custom base_url with an open-weight model: none is preserved",
			config:     &latest.ModelConfig{Provider: "openai", Model: "mlx-community/Qwen3.6-35B-A3B-8bit", BaseURL: "http://localhost:8080/v1", ThinkingBudget: &latest.ThinkingBudget{Effort: "none"}},
			wantBudget: &latest.ThinkingBudget{Effort: "none"},
		},
		{
			name:       "custom base_url with an open-weight model: 0 is preserved",
			config:     &latest.ModelConfig{Provider: "openai", Model: "qwen3", BaseURL: "http://localhost:8080/v1", ThinkingBudget: &latest.ThinkingBudget{Tokens: 0}},
			wantBudget: &latest.ThinkingBudget{Tokens: 0},
		},
		{
			name:       "alias with overridden base_url and an open-weight model: none is preserved",
			config:     &latest.ModelConfig{Provider: "xai", Model: "qwen3", BaseURL: "https://proxy.example.com/v1", ThinkingBudget: &latest.ThinkingBudget{Effort: "none"}},
			wantBudget: &latest.ThinkingBudget{Effort: "none"},
		},
		{
			name:   "custom base_url with an OpenAI model behind a proxy: none becomes nil",
			config: &latest.ModelConfig{Provider: "openai", Model: "gpt-4o", BaseURL: "https://proxy.example.com/v1", ThinkingBudget: &latest.ThinkingBudget{Effort: "none"}},
		},
		{
			name:   "alias default base_url: none becomes nil",
			config: &latest.ModelConfig{Provider: "groq", Model: "qwen/qwen3-32b", BaseURL: "https://api.groq.com/openai/v1", ThinkingBudget: &latest.ThinkingBudget{Effort: "none"}},
		},
		{
			name:   "alias default base_url with trailing slash: none becomes nil",
			config: &latest.ModelConfig{Provider: "groq", Model: "qwen/qwen3-32b", BaseURL: "https://api.groq.com/openai/v1/", ThinkingBudget: &latest.ThinkingBudget{Effort: "none"}},
		},
		{
			name:   "azure deployment (base_url always set, arbitrary deployment name): none becomes nil",
			config: &latest.ModelConfig{Provider: "azure", Model: "my-gpt4o-deployment", BaseURL: "https://acme.openai.azure.com", ThinkingBudget: &latest.ThinkingBudget{Effort: "none"}},
		},
		{
			name:   "custom base_url on the Responses API: none becomes nil (switch is Chat Completions only)",
			config: &latest.ModelConfig{Provider: "vllm", Model: "qwen3", BaseURL: "http://localhost:8000/v1", ProviderOpts: map[string]any{"api_type": "openai_responses"}, ThinkingBudget: &latest.ThinkingBudget{Effort: "none"}},
		},
		{
			name:   "custom provider pointed at azure: none becomes nil",
			config: &latest.ModelConfig{Provider: "azure", Model: "qwen-deployment", BaseURL: "https://acme.openai.azure.com", ProviderOpts: map[string]any{"api_type": "openai_chatcompletions"}, ThinkingBudget: &latest.ThinkingBudget{Effort: "none"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			applyModelDefaults(tt.config)

			// Check thinking budget.
			if tt.wantBudget == nil {
				assert.Nil(t, tt.config.ThinkingBudget)
			} else {
				require.NotNil(t, tt.config.ThinkingBudget)
				assert.Equal(t, *tt.wantBudget, *tt.config.ThinkingBudget)
			}

			// Check interleaved_thinking.
			if tt.wantInterleaved == nil {
				if tt.config.ProviderOpts != nil {
					_, exists := tt.config.ProviderOpts["interleaved_thinking"]
					assert.False(t, exists, "interleaved_thinking should not be set")
				}
			} else {
				require.NotNil(t, tt.config.ProviderOpts)
				assert.Equal(t, *tt.wantInterleaved, tt.config.ProviderOpts["interleaved_thinking"])
			}
		})
	}
}

func TestApplyProviderDefaults(t *testing.T) {
	t.Parallel()

	boolPtr := func(v bool) *bool { return &v }

	tests := []struct {
		name            string
		config          *latest.ModelConfig
		customProviders map[string]latest.ProviderConfig
		wantBudget      *latest.ThinkingBudget
		wantInterleaved *bool
	}{
		{
			name:       "openai o3-mini: thinking-only gets default through provider defaults",
			config:     &latest.ModelConfig{Provider: "openai", Model: "o3-mini"},
			wantBudget: &latest.ThinkingBudget{Effort: "medium"},
		},
		{
			name:   "openai gpt-4o: no default through provider defaults",
			config: &latest.ModelConfig{Provider: "openai", Model: "gpt-4o"},
		},
		{
			name:            "anthropic with explicit budget gets interleaved through provider defaults",
			config:          &latest.ModelConfig{Provider: "anthropic", Model: "claude-sonnet-4-0", ThinkingBudget: &latest.ThinkingBudget{Tokens: 8192}},
			wantBudget:      &latest.ThinkingBudget{Tokens: 8192},
			wantInterleaved: boolPtr(true),
		},
		{
			name:   "custom openai provider: no default thinking",
			config: &latest.ModelConfig{Provider: "my_gateway", Model: "gpt-4o"},
			customProviders: map[string]latest.ProviderConfig{
				"my_gateway": {APIType: "openai_chatcompletions", BaseURL: "https://api.example.com/v1", TokenKey: "MY_KEY"},
			},
		},
		{
			name:       "explicit thinking preserved unchanged",
			config:     &latest.ModelConfig{Provider: "openai", Model: "gpt-4o", ThinkingBudget: &latest.ThinkingBudget{Effort: "high"}},
			wantBudget: &latest.ThinkingBudget{Effort: "high"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := applyProviderDefaults(tt.config, tt.customProviders)

			if tt.wantBudget == nil {
				assert.Nil(t, result.ThinkingBudget)
			} else {
				require.NotNil(t, result.ThinkingBudget)
				assert.Equal(t, *tt.wantBudget, *result.ThinkingBudget)
			}

			if tt.wantInterleaved != nil {
				require.NotNil(t, result.ProviderOpts)
				assert.Equal(t, *tt.wantInterleaved, result.ProviderOpts["interleaved_thinking"])
			}
		})
	}
}

// TestApplyProviderDefaults_DoesNotModifyOriginal verifies that applyProviderDefaults
// does not mutate the input config's ProviderOpts map.
func TestApplyProviderDefaults_DoesNotModifyOriginal(t *testing.T) {
	t.Parallel()

	original := &latest.ModelConfig{
		Provider:       "anthropic",
		Model:          "claude-sonnet-4-0",
		ThinkingBudget: &latest.ThinkingBudget{Tokens: 8192},
		ProviderOpts:   map[string]any{"custom_key": "original_value"},
	}

	result := applyProviderDefaults(original, nil)

	// Result should have interleaved_thinking set (because thinking_budget is set).
	require.NotNil(t, result.ProviderOpts)
	assert.Equal(t, true, result.ProviderOpts["interleaved_thinking"])

	// Original must NOT have interleaved_thinking added.
	_, exists := original.ProviderOpts["interleaved_thinking"]
	assert.False(t, exists, "original ProviderOpts must not be mutated by applyProviderDefaults")

	// Original custom key must still be there.
	assert.Equal(t, "original_value", original.ProviderOpts["custom_key"])
}

// TestApplyProviderDefaults_InheritsAuthFromProviderConfig verifies that a
// ProviderConfig's Auth block is inherited by models that don't override it,
// while a model-level Auth always wins.
func TestApplyProviderDefaults_InheritsAuthFromProviderConfig(t *testing.T) {
	t.Parallel()

	provAuth := &latest.AuthConfig{
		Type: latest.AuthTypeWorkloadIdentityFederation,
		Federation: &latest.FederationAuthConfig{
			FederationRuleID: "fdrl_provider",
			OrganizationID:   "org",
			IdentityToken:    &latest.IdentityTokenSourceConfig{File: "/p"},
		},
	}
	modelAuth := &latest.AuthConfig{
		Type: latest.AuthTypeWorkloadIdentityFederation,
		Federation: &latest.FederationAuthConfig{
			FederationRuleID: "fdrl_model",
			OrganizationID:   "org",
			IdentityToken:    &latest.IdentityTokenSourceConfig{File: "/m"},
		},
	}
	custom := map[string]latest.ProviderConfig{
		"claude": {Provider: "anthropic", Auth: provAuth},
	}

	t.Run("inherits when model has no auth", func(t *testing.T) {
		t.Parallel()
		m := &latest.ModelConfig{Provider: "claude", Model: "claude-x"}
		res := applyProviderDefaults(m, custom)
		require.NotNil(t, res.Auth)
		assert.Equal(t, "fdrl_provider", res.Auth.Federation.FederationRuleID)
		assert.Nil(t, m.Auth, "original ModelConfig must not be mutated")
	})

	t.Run("model auth wins over provider auth", func(t *testing.T) {
		t.Parallel()
		m := &latest.ModelConfig{Provider: "claude", Model: "claude-x", Auth: modelAuth}
		res := applyProviderDefaults(m, custom)
		require.NotNil(t, res.Auth)
		assert.Equal(t, "fdrl_model", res.Auth.Federation.FederationRuleID)
	})
}
