package provider

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/config/latest"
)

// TestApplyProviderDefaults_CustomProviders tests that custom provider defaults are applied lazily
func TestApplyProviderDefaults_CustomProviders(t *testing.T) {
	t.Parallel()

	customProviders := map[string]latest.ProviderConfig{
		"my_gateway": {
			APIType:  "openai_chatcompletions",
			BaseURL:  "https://api.example.com/v1",
			TokenKey: "MY_GATEWAY_KEY",
		},
	}

	tests := []struct {
		name             string
		modelCfg         *latest.ModelConfig
		expectedBaseURL  string
		expectedTokenKey string
		expectedAPIType  string
	}{
		{
			name: "applies all defaults from custom provider",
			modelCfg: &latest.ModelConfig{
				Provider: "my_gateway",
				Model:    "gpt-4o",
			},
			expectedBaseURL:  "https://api.example.com/v1",
			expectedTokenKey: "MY_GATEWAY_KEY",
			expectedAPIType:  "openai_chatcompletions",
		},
		{
			name: "model base_url takes precedence",
			modelCfg: &latest.ModelConfig{
				Provider: "my_gateway",
				Model:    "gpt-4o",
				BaseURL:  "https://override.example.com/v1",
			},
			expectedBaseURL:  "https://override.example.com/v1",
			expectedTokenKey: "MY_GATEWAY_KEY",
			expectedAPIType:  "openai_chatcompletions",
		},
		{
			name: "model token_key takes precedence",
			modelCfg: &latest.ModelConfig{
				Provider: "my_gateway",
				Model:    "gpt-4o",
				TokenKey: "OVERRIDE_KEY",
			},
			expectedBaseURL:  "https://api.example.com/v1",
			expectedTokenKey: "OVERRIDE_KEY",
			expectedAPIType:  "openai_chatcompletions",
		},
		{
			name: "model api_type takes precedence",
			modelCfg: &latest.ModelConfig{
				Provider: "my_gateway",
				Model:    "gpt-4o",
				ProviderOpts: map[string]any{
					"api_type": "openai_responses",
				},
			},
			expectedBaseURL:  "https://api.example.com/v1",
			expectedTokenKey: "MY_GATEWAY_KEY",
			expectedAPIType:  "openai_responses",
		},
		{
			name: "unknown provider returns unchanged config",
			modelCfg: &latest.ModelConfig{
				Provider: "unknown_provider",
				Model:    "test-model",
			},
			expectedBaseURL:  "",
			expectedTokenKey: "",
			expectedAPIType:  "", // No api_type set
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := applyProviderDefaults(tt.modelCfg, customProviders)

			assert.Equal(t, tt.expectedBaseURL, result.BaseURL)
			assert.Equal(t, tt.expectedTokenKey, result.TokenKey)

			if tt.expectedAPIType != "" {
				assert.Equal(t, tt.expectedAPIType, result.ProviderOpts["api_type"])
			} else if result.ProviderOpts != nil {
				// ProviderOpts exists but api_type should not be set
				_, hasAPIType := result.ProviderOpts["api_type"]
				assert.False(t, hasAPIType, "api_type should not be set")
			}
		})
	}
}

// TestApplyProviderDefaults_DefaultsAPIType tests that empty api_type defaults to openai_chatcompletions
func TestApplyProviderDefaults_DefaultsAPIType(t *testing.T) {
	t.Parallel()

	customProviders := map[string]latest.ProviderConfig{
		"no_api_type": {
			BaseURL:  "https://api.example.com/v1",
			TokenKey: "KEY",
			// APIType is empty
		},
	}

	modelCfg := &latest.ModelConfig{
		Provider: "no_api_type",
		Model:    "test",
	}

	result := applyProviderDefaults(modelCfg, customProviders)
	assert.Equal(t, "openai_chatcompletions", result.ProviderOpts["api_type"])
}

// TestApplyProviderDefaults_DefaultsResponsesAPIForNewerModels verifies that a
// custom OpenAI-compatible provider with no explicit api_type defaults to the
// Responses API for models that require it (gpt-5, Codex, o-series), while
// older chat models stay on Chat Completions. Without this, a provider pointed
// at the OpenAI/GitHub Copilot API rejects newer models on /chat/completions
// with a 400. See https://github.com/docker/docker-agent/issues/2303.
func TestApplyProviderDefaults_DefaultsResponsesAPIForNewerModels(t *testing.T) {
	t.Parallel()

	customProviders := map[string]latest.ProviderConfig{
		// OpenAI-compatible provider (e.g. a github-copilot endpoint defined in
		// the providers: section) without an explicit api_type.
		"copilot": {
			BaseURL:  "https://api.githubcopilot.com",
			TokenKey: "GITHUB_TOKEN",
		},
	}

	tests := []struct {
		name            string
		model           string
		expectedAPIType string
	}{
		{name: "codex routes to responses", model: "gpt-5.3-codex", expectedAPIType: "openai_responses"},
		{name: "gpt-5 routes to responses", model: "gpt-5", expectedAPIType: "openai_responses"},
		{name: "o-series routes to responses", model: "o3-mini", expectedAPIType: "openai_responses"},
		{name: "gpt-4o stays on chat completions", model: "gpt-4o", expectedAPIType: "openai_chatcompletions"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := applyProviderDefaults(&latest.ModelConfig{Provider: "copilot", Model: tt.model}, customProviders)
			assert.Equal(t, tt.expectedAPIType, result.ProviderOpts["api_type"])
		})
	}
}

// TestApplyProviderDefaults_ExplicitAPITypeWinsOverModel verifies that an
// explicit provider-level api_type is honored even for a model that would
// otherwise default to the Responses API.
func TestApplyProviderDefaults_ExplicitAPITypeWinsOverModel(t *testing.T) {
	t.Parallel()

	customProviders := map[string]latest.ProviderConfig{
		"pinned": {
			APIType:  "openai_chatcompletions",
			BaseURL:  "https://api.example.com/v1",
			TokenKey: "KEY",
		},
	}

	result := applyProviderDefaults(&latest.ModelConfig{Provider: "pinned", Model: "gpt-5.3-codex"}, customProviders)
	assert.Equal(t, "openai_chatcompletions", result.ProviderOpts["api_type"])
}

// TestResolveProviderTypeFromConfig tests the provider type resolution logic
func TestResolveProviderTypeFromConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		config   *latest.ModelConfig
		expected string
	}{
		{
			name: "api_type from ProviderOpts takes priority",
			config: &latest.ModelConfig{
				Provider:     "openai",
				ProviderOpts: map[string]any{"api_type": "openai_responses"},
			},
			expected: "openai_responses",
		},
		{
			name: "built-in alias takes second priority",
			config: &latest.ModelConfig{
				Provider: "mistral", // Has alias with APIType: "openai"
			},
			expected: "openai",
		},
		{
			name: "provider name is fallback",
			config: &latest.ModelConfig{
				Provider: "anthropic",
			},
			expected: "anthropic",
		},
		{
			name: "custom provider with api_type",
			config: &latest.ModelConfig{
				Provider:     "my_custom_provider",
				ProviderOpts: map[string]any{"api_type": "openai_chatcompletions"},
			},
			expected: "openai_chatcompletions",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, resolveProviderType(tt.config))
		})
	}
}
