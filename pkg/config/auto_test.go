package config

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

func TestAvailableProviders_NoGateway(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		envVars          map[string]string
		expectedProvider string
	}{
		{
			name: "anthropic api key present",
			envVars: map[string]string{
				"ANTHROPIC_API_KEY": "test-key",
			},
			expectedProvider: "anthropic",
		},
		{
			name: "openai api key present",
			envVars: map[string]string{
				"OPENAI_API_KEY": "test-key",
			},
			expectedProvider: "openai",
		},
		{
			name: "google api key present",
			envVars: map[string]string{
				"GOOGLE_API_KEY": "test-key",
			},
			expectedProvider: "google",
		},
		{
			name: "mistral api key present",
			envVars: map[string]string{
				"MISTRAL_API_KEY": "test-key",
			},
			expectedProvider: "mistral",
		},
		{
			name:             "no api keys - defaults to dmr",
			envVars:          map[string]string{},
			expectedProvider: "dmr",
		},
		{
			name: "anthropic takes precedence over openai",
			envVars: map[string]string{
				"ANTHROPIC_API_KEY": "test-key",
				"OPENAI_API_KEY":    "test-key",
			},
			expectedProvider: "anthropic",
		},
		{
			name: "openai takes precedence over google",
			envVars: map[string]string{
				"OPENAI_API_KEY": "test-key",
				"GOOGLE_API_KEY": "test-key",
			},
			expectedProvider: "openai",
		},
		{
			name: "google takes precedence over mistral",
			envVars: map[string]string{
				"GOOGLE_API_KEY":  "test-key",
				"MISTRAL_API_KEY": "test-key",
			},
			expectedProvider: "google",
		},
		{
			name: "mistral takes precedence over dmr",
			envVars: map[string]string{
				"MISTRAL_API_KEY": "test-key",
			},
			expectedProvider: "mistral",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			providers := AvailableProviders(t.Context(), "", environment.NewMapEnvProvider(tt.envVars))

			assert.NotEmpty(t, providers)
			assert.Equal(t, tt.expectedProvider, providers[0])
		})
	}
}

func TestAvailableProviders_WithGateway(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		envVars          map[string]string
		gateway          string
		expectedProvider string
	}{
		{
			name:             "gateway present with no api keys",
			envVars:          map[string]string{},
			gateway:          "gateway:8080",
			expectedProvider: "anthropic",
		},
		{
			name: "gateway present with anthropic api key",
			envVars: map[string]string{
				"ANTHROPIC_API_KEY": "test-key",
			},
			gateway:          "gateway:8080",
			expectedProvider: "anthropic",
		},
		{
			name: "gateway present with openai api key",
			envVars: map[string]string{
				"OPENAI_API_KEY": "test-key",
			},
			gateway:          "gateway:8080",
			expectedProvider: "anthropic",
		},
		{
			name: "gateway present with all api keys",
			envVars: map[string]string{
				"ANTHROPIC_API_KEY": "test-key",
				"OPENAI_API_KEY":    "test-key",
				"GOOGLE_API_KEY":    "test-key",
				"MISTRAL_API_KEY":   "test-key",
			},
			gateway:          "gateway:8080",
			expectedProvider: "anthropic",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			providers := AvailableProviders(t.Context(), tt.gateway, environment.NewMapEnvProvider(tt.envVars))

			assert.Len(t, providers, 1)
			assert.Equal(t, tt.expectedProvider, providers[0])
		})
	}
}

func TestAutoModelConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		envVars           map[string]string
		gateway           string
		expectedProvider  string
		expectedModel     string
		expectedMaxTokens int
	}{
		{
			name: "anthropic provider",
			envVars: map[string]string{
				"ANTHROPIC_API_KEY": "test-key",
			},
			expectedProvider:  "anthropic",
			expectedModel:     "claude-sonnet-4-6",
			expectedMaxTokens: 32000,
		},
		{
			name: "openai provider",
			envVars: map[string]string{
				"OPENAI_API_KEY": "test-key",
			},
			expectedProvider:  "openai",
			expectedModel:     "gpt-5",
			expectedMaxTokens: 32000,
		},
		{
			name: "google provider",
			envVars: map[string]string{
				"GOOGLE_API_KEY": "test-key",
			},
			expectedProvider:  "google",
			expectedModel:     "gemini-3.5-flash",
			expectedMaxTokens: 32000,
		},
		{
			name: "mistral provider",
			envVars: map[string]string{
				"MISTRAL_API_KEY": "test-key",
			},
			expectedProvider:  "mistral",
			expectedModel:     "mistral-small-latest",
			expectedMaxTokens: 32000,
		},
		{
			name:              "dmr provider (no api keys)",
			envVars:           map[string]string{},
			expectedProvider:  "dmr",
			expectedModel:     "ai/qwen3:latest",
			expectedMaxTokens: 16000,
		},
		{
			name:              "gateway defaults to anthropic",
			envVars:           map[string]string{},
			gateway:           "gateway:8080",
			expectedProvider:  "anthropic",
			expectedModel:     "claude-sonnet-4-6",
			expectedMaxTokens: 32000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			modelConfig := AutoModelConfig(t.Context(), tt.gateway, environment.NewMapEnvProvider(tt.envVars), nil, nil)

			assert.Equal(t, tt.expectedProvider, modelConfig.Provider)
			assert.Equal(t, tt.expectedModel, modelConfig.Model)
			assert.Equal(t, int64(tt.expectedMaxTokens), *modelConfig.MaxTokens)
		})
	}
}

func TestPreferredMaxTokens(t *testing.T) {
	t.Parallel()

	tests := []struct {
		provider       string
		expectedTokens int64
	}{
		{
			provider:       "dmr",
			expectedTokens: 16000,
		},
		{
			provider:       "openai",
			expectedTokens: 32000,
		},
		{
			provider:       "anthropic",
			expectedTokens: 32000,
		},
		{
			provider:       "google",
			expectedTokens: 32000,
		},
		{
			provider:       "mistral",
			expectedTokens: 32000,
		},
		{
			provider:       "unknown-provider",
			expectedTokens: 32000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			t.Parallel()

			tokens := PreferredMaxTokens(tt.provider)

			assert.Equal(t, tt.expectedTokens, *tokens)
		})
	}
}

func TestDefaultModels(t *testing.T) {
	t.Parallel()

	// Test that DefaultModels map has all expected providers
	expectedProviders := []string{"openai", "anthropic", "google", "dmr", "mistral", "amazon-bedrock"}

	for _, provider := range expectedProviders {
		t.Run(provider, func(t *testing.T) {
			model, exists := DefaultModels[provider]
			assert.True(t, exists, "Provider %s should exist in DefaultModels", provider)
			assert.NotEmpty(t, model, "Model for provider %s should not be empty", provider)
		})
	}

	// Test specific model values
	assert.Equal(t, "gpt-5", DefaultModels["openai"])
	assert.Equal(t, "claude-sonnet-4-6", DefaultModels["anthropic"])
	assert.Equal(t, "gemini-3.5-flash", DefaultModels["google"])
	assert.Equal(t, "ai/qwen3:latest", DefaultModels["dmr"])
	assert.Equal(t, "mistral-small-latest", DefaultModels["mistral"])
	assert.Equal(t, "global.anthropic.claude-sonnet-4-5-20250929-v1:0", DefaultModels["amazon-bedrock"])
}

func TestAutoModelConfig_IntegrationWithDefaultModels(t *testing.T) {
	t.Parallel()

	// Verify that AutoModelConfig always returns a model from DefaultModels
	providers := []string{"openai", "anthropic", "google", "mistral"}

	for _, provider := range providers {
		t.Run(provider, func(t *testing.T) {
			t.Parallel()

			envVars := map[string]string{}

			// Set the appropriate API key for the provider
			switch provider {
			case "openai":
				envVars["OPENAI_API_KEY"] = "test-key"
			case "anthropic":
				envVars["ANTHROPIC_API_KEY"] = "test-key"
			case "google":
				envVars["GOOGLE_API_KEY"] = "test-key"
			case "mistral":
				envVars["MISTRAL_API_KEY"] = "test-key"
			}

			modelConfig := AutoModelConfig(t.Context(), "", environment.NewMapEnvProvider(envVars), nil, nil)

			// Verify the returned model matches the DefaultModels entry
			expectedModel := DefaultModels[provider]
			assert.Equal(t, expectedModel, modelConfig.Model)
			assert.Equal(t, provider, modelConfig.Provider)
		})
	}

	// Test dmr provider (no API keys)
	t.Run("dmr", func(t *testing.T) {
		t.Parallel()

		modelConfig := AutoModelConfig(t.Context(), "", environment.NewNoEnvProvider(), nil, nil)

		assert.Equal(t, "dmr", modelConfig.Provider)
		assert.Equal(t, DefaultModels["dmr"], modelConfig.Model)
		assert.Equal(t, int64(16000), *modelConfig.MaxTokens)
	})
}

func TestAvailableProviders_PrecedenceOrder(t *testing.T) {
	t.Parallel()

	// All keys present - anthropic should win
	var env environment.Provider = environment.NewMapEnvProvider(map[string]string{
		"ANTHROPIC_API_KEY": "test-key",
		"OPENAI_API_KEY":    "test-key",
		"GOOGLE_API_KEY":    "test-key",
		"MISTRAL_API_KEY":   "test-key",
	})
	providers := AvailableProviders(t.Context(), "", env)
	assert.Equal(t, "anthropic", providers[0])

	// No anthropic - openai should win
	env = environment.NewMapEnvProvider(map[string]string{
		"OPENAI_API_KEY":  "test-key",
		"GOOGLE_API_KEY":  "test-key",
		"MISTRAL_API_KEY": "test-key",
	})
	providers = AvailableProviders(t.Context(), "", env)
	assert.Equal(t, "openai", providers[0])

	// No anthropic or openai - google should win
	env = environment.NewMapEnvProvider(map[string]string{
		"GOOGLE_API_KEY":  "test-key",
		"MISTRAL_API_KEY": "test-key",
	})
	providers = AvailableProviders(t.Context(), "", env)
	assert.Equal(t, "google", providers[0])

	// No anthropic, openai, or google - mistral should win
	env = environment.NewMapEnvProvider(map[string]string{
		"MISTRAL_API_KEY": "test-key",
	})
	providers = AvailableProviders(t.Context(), "", env)
	assert.Equal(t, "mistral", providers[0])

	// No keys at all - dmr should be selected
	env = environment.NewNoEnvProvider()
	providers = AvailableProviders(t.Context(), "", env)
	assert.Equal(t, "dmr", providers[0])
}

func TestAutoModelConfig_UserDefaultModel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		defaultModel      *latest.ModelConfig
		envVars           map[string]string
		expectedProvider  string
		expectedModel     string
		expectedMaxTokens int64
	}{
		{
			name:              "user default model overrides auto detection",
			defaultModel:      &latest.ModelConfig{Provider: "openai", Model: "gpt-4o"},
			envVars:           map[string]string{"ANTHROPIC_API_KEY": "test-key"},
			expectedProvider:  "openai",
			expectedModel:     "gpt-4o",
			expectedMaxTokens: 32000,
		},
		{
			name:              "user default model with dmr provider",
			defaultModel:      &latest.ModelConfig{Provider: "dmr", Model: "ai/llama3.2"},
			envVars:           map[string]string{"OPENAI_API_KEY": "test-key"},
			expectedProvider:  "dmr",
			expectedModel:     "ai/llama3.2",
			expectedMaxTokens: 16000,
		},
		{
			name:              "user default model with anthropic provider",
			defaultModel:      &latest.ModelConfig{Provider: "anthropic", Model: "claude-sonnet-4-0"},
			envVars:           map[string]string{},
			expectedProvider:  "anthropic",
			expectedModel:     "claude-sonnet-4-0",
			expectedMaxTokens: 32000,
		},
		{
			name:              "nil default model falls back to auto detection",
			defaultModel:      nil,
			envVars:           map[string]string{"GOOGLE_API_KEY": "test-key"},
			expectedProvider:  "google",
			expectedModel:     "gemini-3.5-flash",
			expectedMaxTokens: 32000,
		},
		{
			name:              "empty provider falls back to auto detection",
			defaultModel:      &latest.ModelConfig{Provider: "", Model: "model-only"},
			envVars:           map[string]string{"MISTRAL_API_KEY": "test-key"},
			expectedProvider:  "mistral",
			expectedModel:     "mistral-small-latest",
			expectedMaxTokens: 32000,
		},
		{
			name:              "empty model falls back to auto detection",
			defaultModel:      &latest.ModelConfig{Provider: "openai", Model: ""},
			envVars:           map[string]string{"ANTHROPIC_API_KEY": "test-key"},
			expectedProvider:  "anthropic",
			expectedModel:     "claude-sonnet-4-6",
			expectedMaxTokens: 32000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			modelConfig := AutoModelConfig(t.Context(), "", environment.NewMapEnvProvider(tt.envVars), tt.defaultModel, nil)

			assert.Equal(t, tt.expectedProvider, modelConfig.Provider)
			assert.Equal(t, tt.expectedModel, modelConfig.Model)
			assert.Equal(t, tt.expectedMaxTokens, *modelConfig.MaxTokens)
		})
	}
}

func TestAutoModelConfig_UserDefaultModelWithOptions(t *testing.T) {
	t.Parallel()

	// Test that user-provided options like max_tokens, thinking_budget are preserved
	customMaxTokens := int64(64000)
	thinkingBudget := &latest.ThinkingBudget{Tokens: 10000}

	defaultModel := &latest.ModelConfig{
		Provider:       "anthropic",
		Model:          "claude-sonnet-4-5",
		MaxTokens:      &customMaxTokens,
		ThinkingBudget: thinkingBudget,
	}

	modelConfig := AutoModelConfig(t.Context(), "", environment.NewNoEnvProvider(), defaultModel, nil)

	assert.Equal(t, "anthropic", modelConfig.Provider)
	assert.Equal(t, "claude-sonnet-4-5", modelConfig.Model)
	assert.Equal(t, int64(64000), *modelConfig.MaxTokens)
	assert.NotNil(t, modelConfig.ThinkingBudget)
	assert.Equal(t, 10000, modelConfig.ThinkingBudget.Tokens)
}

func TestAutoModelConfig_DMRLocalModels(t *testing.T) {
	t.Parallel()

	lister := func(models []string, err error) DMRModelLister {
		return func(context.Context) ([]string, error) { return models, err }
	}

	tests := []struct {
		name          string
		lister        DMRModelLister
		expectedModel string
	}{
		{
			name:          "nil lister keeps the static default",
			lister:        nil,
			expectedModel: DefaultModels["dmr"],
		},
		{
			name:          "default model already pulled is used",
			lister:        lister([]string{"ai/gemma3:latest", "ai/qwen3:latest"}, nil),
			expectedModel: "ai/qwen3:latest",
		},
		{
			name:          "default not pulled falls back to first installed model",
			lister:        lister([]string{"ai/llama3.2:latest", "ai/smollm2:latest"}, nil),
			expectedModel: "ai/llama3.2:latest",
		},
		{
			name:          "default model under a different tag is preferred over other models",
			lister:        lister([]string{"ai/gemma3:latest", "ai/qwen3:Q4_K_M"}, nil),
			expectedModel: "ai/qwen3:Q4_K_M",
		},
		{
			name:          "embedding-only models are skipped, default retained",
			lister:        lister([]string{"ai/embeddinggemma", "ai/nomic-embed-text"}, nil),
			expectedModel: DefaultModels["dmr"],
		},
		{
			name:          "embedding models are skipped when a chat model exists",
			lister:        lister([]string{"ai/embeddinggemma", "ai/mistral:latest"}, nil),
			expectedModel: "ai/mistral:latest",
		},
		{
			name:          "discovery error keeps the static default",
			lister:        lister(nil, errors.New("dmr unreachable")),
			expectedModel: DefaultModels["dmr"],
		},
		{
			name:          "empty list keeps the static default",
			lister:        lister([]string{}, nil),
			expectedModel: DefaultModels["dmr"],
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			modelConfig := AutoModelConfig(t.Context(), "", environment.NewNoEnvProvider(), nil, tt.lister)

			assert.Equal(t, "dmr", modelConfig.Provider)
			assert.Equal(t, tt.expectedModel, modelConfig.Model)
			assert.Equal(t, int64(16000), *modelConfig.MaxTokens)
		})
	}
}

func TestAutoModelConfig_DMRListerNotConsultedForCloudProvider(t *testing.T) {
	t.Parallel()

	called := false
	lister := func(context.Context) ([]string, error) {
		called = true
		return []string{"ai/qwen3:latest"}, nil
	}

	// A cloud provider is available, so the DMR lister must never run.
	modelConfig := AutoModelConfig(
		t.Context(),
		"",
		environment.NewMapEnvProvider(map[string]string{"ANTHROPIC_API_KEY": "test-key"}),
		nil,
		lister,
	)

	assert.Equal(t, "anthropic", modelConfig.Provider)
	assert.False(t, called, "DMR lister should not be consulted when a cloud provider is selected")
}

func TestAutoModelFallbackError(t *testing.T) {
	t.Parallel()

	t.Run("without cause", func(t *testing.T) {
		t.Parallel()

		err := &AutoModelFallbackError{}
		msg := err.Error()
		assert.Contains(t, msg, "No model is currently available")
		assert.Contains(t, msg, "docker model pull")
		assert.Contains(t, msg, "ANTHROPIC_API_KEY")
		assert.NotContains(t, msg, "Could not initialize")
	})

	t.Run("with cause is surfaced and unwrappable", func(t *testing.T) {
		t.Parallel()

		cause := errors.New("model pull declined by user")
		err := &AutoModelFallbackError{Cause: cause}
		assert.Contains(t, err.Error(), "model pull declined by user")
		assert.ErrorIs(t, err, cause)
	})
}
