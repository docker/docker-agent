package config

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chatgpt"
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
			name: "github copilot github token present",
			envVars: map[string]string{
				"GITHUB_TOKEN": "test-token",
			},
			expectedProvider: "github-copilot",
		},
		{
			name: "github copilot gh token present",
			envVars: map[string]string{
				"GH_TOKEN": "test-token",
			},
			expectedProvider: "github-copilot",
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
			name: "openrouter api key present",
			envVars: map[string]string{
				"OPENROUTER_API_KEY": "test-key",
			},
			expectedProvider: "openrouter",
		},
		{
			name: "orcarouter api key present",
			envVars: map[string]string{
				"ORCAROUTER_API_KEY": "test-key",
			},
			expectedProvider: "orcarouter",
		},
		{
			name: "baseten api key present",
			envVars: map[string]string{
				"BASETEN_API_KEY": "test-key",
			},
			expectedProvider: "baseten",
		},
		{
			name: "ovhcloud access token present",
			envVars: map[string]string{
				"OVH_AI_ENDPOINTS_ACCESS_TOKEN": "test-token",
			},
			expectedProvider: "ovhcloud",
		},
		{
			name: "groq api key present",
			envVars: map[string]string{
				"GROQ_API_KEY": "test-key",
			},
			expectedProvider: "groq",
		},
		{
			name: "fireworks api key present",
			envVars: map[string]string{
				"FIREWORKS_API_KEY": "test-key",
			},
			expectedProvider: "fireworks",
		},
		{
			name: "deepseek api key present",
			envVars: map[string]string{
				"DEEPSEEK_API_KEY": "test-key",
			},
			expectedProvider: "deepseek",
		},
		{
			name: "cerebras api key present",
			envVars: map[string]string{
				"CEREBRAS_API_KEY": "test-key",
			},
			expectedProvider: "cerebras",
		},
		{
			name: "together api key present",
			envVars: map[string]string{
				"TOGETHER_API_KEY": "test-key",
			},
			expectedProvider: "together",
		},
		{
			name: "huggingface token present",
			envVars: map[string]string{
				"HF_TOKEN": "test-token",
			},
			expectedProvider: "huggingface",
		},
		{
			name: "moonshot api key present",
			envVars: map[string]string{
				"MOONSHOT_API_KEY": "test-key",
			},
			expectedProvider: "moonshot",
		},
		{
			name: "vercel ai gateway key present",
			envVars: map[string]string{
				"AI_GATEWAY_API_KEY": "test-key",
			},
			expectedProvider: "vercel",
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
			expectedModel:     "gpt-5.6",
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
			name: "openrouter provider",
			envVars: map[string]string{
				"OPENROUTER_API_KEY": "test-key",
			},
			expectedProvider:  "openrouter",
			expectedModel:     "meta-llama/llama-3.3-70b-instruct",
			expectedMaxTokens: 32000,
		},
		{
			name: "orcarouter provider",
			envVars: map[string]string{
				"ORCAROUTER_API_KEY": "test-key",
			},
			expectedProvider:  "orcarouter",
			expectedModel:     "orcarouter/auto",
			expectedMaxTokens: 32000,
		},
		{
			name: "baseten provider",
			envVars: map[string]string{
				"BASETEN_API_KEY": "test-key",
			},
			expectedProvider:  "baseten",
			expectedModel:     "deepseek-ai/DeepSeek-V3.1",
			expectedMaxTokens: 32000,
		},
		{
			name: "ovhcloud provider",
			envVars: map[string]string{
				"OVH_AI_ENDPOINTS_ACCESS_TOKEN": "test-token",
			},
			expectedProvider:  "ovhcloud",
			expectedModel:     "Qwen3.5-397B-A17B",
			expectedMaxTokens: 32000,
		},
		{
			name: "groq provider",
			envVars: map[string]string{
				"GROQ_API_KEY": "test-key",
			},
			expectedProvider:  "groq",
			expectedModel:     "llama-3.3-70b-versatile",
			expectedMaxTokens: 32000,
		},
		{
			name: "fireworks provider",
			envVars: map[string]string{
				"FIREWORKS_API_KEY": "test-key",
			},
			expectedProvider:  "fireworks",
			expectedModel:     "accounts/fireworks/models/kimi-k2-instruct",
			expectedMaxTokens: 32000,
		},
		{
			name: "deepseek provider",
			envVars: map[string]string{
				"DEEPSEEK_API_KEY": "test-key",
			},
			expectedProvider:  "deepseek",
			expectedModel:     "deepseek-chat",
			expectedMaxTokens: 32000,
		},
		{
			name: "cerebras provider",
			envVars: map[string]string{
				"CEREBRAS_API_KEY": "test-key",
			},
			expectedProvider:  "cerebras",
			expectedModel:     "gpt-oss-120b",
			expectedMaxTokens: 32000,
		},
		{
			name: "together provider",
			envVars: map[string]string{
				"TOGETHER_API_KEY": "test-key",
			},
			expectedProvider:  "together",
			expectedModel:     "meta-llama/Llama-3.3-70B-Instruct-Turbo",
			expectedMaxTokens: 32000,
		},
		{
			name: "huggingface provider",
			envVars: map[string]string{
				"HF_TOKEN": "test-token",
			},
			expectedProvider:  "huggingface",
			expectedModel:     "meta-llama/Llama-3.3-70B-Instruct",
			expectedMaxTokens: 32000,
		},
		{
			name: "moonshot provider",
			envVars: map[string]string{
				"MOONSHOT_API_KEY": "test-key",
			},
			expectedProvider:  "moonshot",
			expectedModel:     "kimi-k2-0905-preview",
			expectedMaxTokens: 32000,
		},
		{
			name: "vercel provider",
			envVars: map[string]string{
				"AI_GATEWAY_API_KEY": "test-key",
			},
			expectedProvider:  "vercel",
			expectedModel:     "openai/gpt-5.6-sol",
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
			provider:       "openrouter",
			expectedTokens: 32000,
		},
		{
			provider:       "orcarouter",
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
	expectedProviders := []string{"openai", "anthropic", "google", "dmr", "mistral", "openrouter", "orcarouter", "baseten", "ovhcloud", "groq", "fireworks", "deepseek", "cerebras", "together", "huggingface", "moonshot", "vercel", "amazon-bedrock", "opencode-zen", "opencode-go", "github-copilot"}

	for _, provider := range expectedProviders {
		t.Run(provider, func(t *testing.T) {
			model, exists := DefaultModels[provider]
			assert.True(t, exists, "Provider %s should exist in DefaultModels", provider)
			assert.NotEmpty(t, model, "Model for provider %s should not be empty", provider)
		})
	}

	// Test specific model values
	assert.Equal(t, "gpt-5.6", DefaultModels["openai"])
	assert.Equal(t, "gpt-5.6", DefaultModels["github-copilot"])
	assert.Equal(t, "claude-sonnet-4-6", DefaultModels["anthropic"])
	assert.Equal(t, "gemini-3.5-flash", DefaultModels["google"])
	assert.Equal(t, "ai/qwen3:latest", DefaultModels["dmr"])
	assert.Equal(t, "mistral-small-latest", DefaultModels["mistral"])
	assert.Equal(t, "meta-llama/llama-3.3-70b-instruct", DefaultModels["openrouter"])
	assert.Equal(t, "orcarouter/auto", DefaultModels["orcarouter"])
	assert.Equal(t, "deepseek-ai/DeepSeek-V3.1", DefaultModels["baseten"])
	assert.Equal(t, "Qwen3.5-397B-A17B", DefaultModels["ovhcloud"])
	assert.Equal(t, "llama-3.3-70b-versatile", DefaultModels["groq"])
	assert.Equal(t, "accounts/fireworks/models/kimi-k2-instruct", DefaultModels["fireworks"])
	assert.Equal(t, "deepseek-chat", DefaultModels["deepseek"])
	assert.Equal(t, "gpt-oss-120b", DefaultModels["cerebras"])
	assert.Equal(t, "meta-llama/Llama-3.3-70B-Instruct-Turbo", DefaultModels["together"])
	assert.Equal(t, "meta-llama/Llama-3.3-70B-Instruct", DefaultModels["huggingface"])
	assert.Equal(t, "kimi-k2-0905-preview", DefaultModels["moonshot"])
	assert.Equal(t, "openai/gpt-5.6-sol", DefaultModels["vercel"])
	assert.Equal(t, "global.anthropic.claude-sonnet-4-5-20250929-v1:0", DefaultModels["amazon-bedrock"])
	assert.Equal(t, "deepseek-v4-flash", DefaultModels["opencode-go"])
	assert.Equal(t, "deepseek-v4-flash-free", DefaultModels["opencode-zen"])
}

func TestAutoModelConfig_IntegrationWithDefaultModels(t *testing.T) {
	t.Parallel()

	// Verify that AutoModelConfig always returns a model from DefaultModels
	providers := []string{"openai", "anthropic", "google", "mistral", "openrouter", "orcarouter", "baseten", "ovhcloud", "groq", "fireworks", "deepseek", "cerebras", "together", "huggingface", "moonshot", "vercel", "opencode-zen", "github-copilot"}

	for _, provider := range providers {
		t.Run(provider, func(t *testing.T) {
			t.Parallel()

			envVars := map[string]string{}

			// Set the appropriate API key for the provider
			switch provider {
			case "openai":
				envVars["OPENAI_API_KEY"] = "test-key"
			case "github-copilot":
				envVars["GITHUB_TOKEN"] = "test-key"
			case "anthropic":
				envVars["ANTHROPIC_API_KEY"] = "test-key"
			case "google":
				envVars["GOOGLE_API_KEY"] = "test-key"
			case "mistral":
				envVars["MISTRAL_API_KEY"] = "test-key"
			case "openrouter":
				envVars["OPENROUTER_API_KEY"] = "test-key"
			case "orcarouter":
				envVars["ORCAROUTER_API_KEY"] = "test-key"
			case "baseten":
				envVars["BASETEN_API_KEY"] = "test-key"
			case "ovhcloud":
				envVars["OVH_AI_ENDPOINTS_ACCESS_TOKEN"] = "test-token"
			case "groq":
				envVars["GROQ_API_KEY"] = "test-key"
			case "fireworks":
				envVars["FIREWORKS_API_KEY"] = "test-key"
			case "deepseek":
				envVars["DEEPSEEK_API_KEY"] = "test-key"
			case "cerebras":
				envVars["CEREBRAS_API_KEY"] = "test-key"
			case "together":
				envVars["TOGETHER_API_KEY"] = "test-key"
			case "huggingface":
				envVars["HF_TOKEN"] = "test-token"
			case "moonshot":
				envVars["MOONSHOT_API_KEY"] = "test-key"
			case "vercel":
				envVars["AI_GATEWAY_API_KEY"] = "test-key"
			case "opencode-zen":
				envVars["OPENCODE_API_KEY"] = "test-key"
			}

			modelConfig := AutoModelConfig(t.Context(), "", environment.NewMapEnvProvider(envVars), nil, nil)

			// Verify the returned model matches the DefaultModels entry
			expectedModel := DefaultModels[provider]
			assert.Equal(t, expectedModel, modelConfig.Model)
			assert.Equal(t, provider, modelConfig.Provider)
		})
	}

	// opencode-go shares OPENCODE_API_KEY with opencode-zen, so it can never be
	// auto-selected when the env var is set (zen wins due to cloudProviders
	// ordering). Test it via a user-specified default model instead.
	t.Run("opencode-go", func(t *testing.T) {
		t.Parallel()

		modelConfig := AutoModelConfig(t.Context(), "", environment.NewMapEnvProvider(map[string]string{
			"OPENCODE_API_KEY": "test-key",
		}), &latest.ModelConfig{Provider: "opencode-go", Model: DefaultModels["opencode-go"]}, nil)

		assert.Equal(t, "opencode-go", modelConfig.Provider)
		assert.Equal(t, DefaultModels["opencode-go"], modelConfig.Model)
	})

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
		"OPENCODE_API_KEY":  "test-key",
	})
	providers := AvailableProviders(t.Context(), "", env)
	assert.Equal(t, "anthropic", providers[0])

	// No anthropic - openai should win
	env = environment.NewMapEnvProvider(map[string]string{
		"OPENAI_API_KEY":   "test-key",
		"GOOGLE_API_KEY":   "test-key",
		"MISTRAL_API_KEY":  "test-key",
		"OPENCODE_API_KEY": "test-key",
	})
	providers = AvailableProviders(t.Context(), "", env)
	assert.Equal(t, "openai", providers[0])

	// No anthropic or openai - google should win
	env = environment.NewMapEnvProvider(map[string]string{
		"GOOGLE_API_KEY":   "test-key",
		"MISTRAL_API_KEY":  "test-key",
		"OPENCODE_API_KEY": "test-key",
	})
	providers = AvailableProviders(t.Context(), "", env)
	assert.Equal(t, "google", providers[0])

	// No anthropic, openai, or google - mistral should win
	env = environment.NewMapEnvProvider(map[string]string{
		"MISTRAL_API_KEY":    "test-key",
		"OPENROUTER_API_KEY": "test-key",
		"OPENCODE_API_KEY":   "test-key",
	})
	providers = AvailableProviders(t.Context(), "", env)
	assert.Equal(t, "mistral", providers[0])

	// No higher-priority providers - openrouter should win before opencode
	env = environment.NewMapEnvProvider(map[string]string{
		"OPENROUTER_API_KEY": "test-key",
		"OPENCODE_API_KEY":   "test-key",
	})
	providers = AvailableProviders(t.Context(), "", env)
	assert.Equal(t, "openrouter", providers[0])

	// openrouter wins over baseten
	env = environment.NewMapEnvProvider(map[string]string{
		"OPENROUTER_API_KEY": "test-key",
		"BASETEN_API_KEY":    "test-key",
	})
	providers = AvailableProviders(t.Context(), "", env)
	assert.Equal(t, "openrouter", providers[0])

	// baseten wins over opencode
	env = environment.NewMapEnvProvider(map[string]string{
		"BASETEN_API_KEY":  "test-key",
		"OPENCODE_API_KEY": "test-key",
	})
	providers = AvailableProviders(t.Context(), "", env)
	assert.Equal(t, "baseten", providers[0])

	// baseten wins over ovhcloud
	env = environment.NewMapEnvProvider(map[string]string{
		"BASETEN_API_KEY":               "test-key",
		"OVH_AI_ENDPOINTS_ACCESS_TOKEN": "test-token",
	})
	providers = AvailableProviders(t.Context(), "", env)
	assert.Equal(t, "baseten", providers[0])

	// ovhcloud wins over groq
	env = environment.NewMapEnvProvider(map[string]string{
		"OVH_AI_ENDPOINTS_ACCESS_TOKEN": "test-token",
		"GROQ_API_KEY":                  "test-key",
	})
	providers = AvailableProviders(t.Context(), "", env)
	assert.Equal(t, "ovhcloud", providers[0])

	// groq wins over fireworks
	env = environment.NewMapEnvProvider(map[string]string{
		"GROQ_API_KEY":      "test-key",
		"FIREWORKS_API_KEY": "test-key",
	})
	providers = AvailableProviders(t.Context(), "", env)
	assert.Equal(t, "groq", providers[0])

	// fireworks wins over deepseek
	env = environment.NewMapEnvProvider(map[string]string{
		"FIREWORKS_API_KEY": "test-key",
		"DEEPSEEK_API_KEY":  "test-key",
	})
	providers = AvailableProviders(t.Context(), "", env)
	assert.Equal(t, "fireworks", providers[0])

	// deepseek wins over cerebras
	env = environment.NewMapEnvProvider(map[string]string{
		"DEEPSEEK_API_KEY": "test-key",
		"CEREBRAS_API_KEY": "test-key",
	})
	providers = AvailableProviders(t.Context(), "", env)
	assert.Equal(t, "deepseek", providers[0])

	// cerebras wins over together
	env = environment.NewMapEnvProvider(map[string]string{
		"CEREBRAS_API_KEY": "test-key",
		"TOGETHER_API_KEY": "test-key",
	})
	providers = AvailableProviders(t.Context(), "", env)
	assert.Equal(t, "cerebras", providers[0])

	// together wins over huggingface
	env = environment.NewMapEnvProvider(map[string]string{
		"TOGETHER_API_KEY": "test-key",
		"HF_TOKEN":         "test-token",
	})
	providers = AvailableProviders(t.Context(), "", env)
	assert.Equal(t, "together", providers[0])

	// huggingface wins over moonshot
	env = environment.NewMapEnvProvider(map[string]string{
		"HF_TOKEN":         "test-token",
		"MOONSHOT_API_KEY": "test-key",
	})
	providers = AvailableProviders(t.Context(), "", env)
	assert.Equal(t, "huggingface", providers[0])

	// moonshot wins over vercel
	env = environment.NewMapEnvProvider(map[string]string{
		"MOONSHOT_API_KEY":   "test-key",
		"AI_GATEWAY_API_KEY": "test-key",
	})
	providers = AvailableProviders(t.Context(), "", env)
	assert.Equal(t, "moonshot", providers[0])

	// vercel wins over amazon-bedrock
	env = environment.NewMapEnvProvider(map[string]string{
		"AI_GATEWAY_API_KEY": "test-key",
		"AWS_ACCESS_KEY_ID":  "test-key",
	})
	providers = AvailableProviders(t.Context(), "", env)
	assert.Equal(t, "vercel", providers[0])

	// Only OPENCODE_API_KEY set - opencode-zen should win (higher priority than opencode-go)
	env = environment.NewMapEnvProvider(map[string]string{
		"OPENCODE_API_KEY": "test-key",
	})
	providers = AvailableProviders(t.Context(), "", env)
	assert.Equal(t, "opencode-zen", providers[0])

	// No keys at all - dmr should be selected
	env = environment.NewNoEnvProvider()
	providers = AvailableProviders(t.Context(), "", env)
	assert.Equal(t, "dmr", providers[0])
}

func TestAvailableProviders_ChatGPTLogin(t *testing.T) {
	t.Parallel()

	// A ChatGPT sign-in (served as the virtual CHATGPT_OAUTH_TOKEN variable)
	// makes the chatgpt provider available.
	var env environment.Provider = environment.NewMapEnvProvider(map[string]string{
		"CHATGPT_OAUTH_TOKEN": "access-token",
	})
	providers := AvailableProviders(t.Context(), "", env)
	assert.Equal(t, "chatgpt", providers[0])

	// An explicit OPENAI_API_KEY keeps priority so adding a sign-in never
	// changes auto-selection for existing API-key users.
	env = environment.NewMapEnvProvider(map[string]string{
		"OPENAI_API_KEY":      "test-key",
		"CHATGPT_OAUTH_TOKEN": "access-token",
	})
	providers = AvailableProviders(t.Context(), "", env)
	assert.Equal(t, "openai", providers[0])

	// The sign-in wins over lower-priority providers.
	env = environment.NewMapEnvProvider(map[string]string{
		"CHATGPT_OAUTH_TOKEN": "access-token",
		"MISTRAL_API_KEY":     "test-key",
	})
	providers = AvailableProviders(t.Context(), "", env)
	assert.Equal(t, "chatgpt", providers[0])
}

func TestAutoModelConfig_ChatGPT(t *testing.T) {
	t.Parallel()

	env := environment.NewMapEnvProvider(map[string]string{"CHATGPT_OAUTH_TOKEN": "access-token"})

	modelConfig := AutoModelConfig(t.Context(), "", env, nil, nil)

	assert.Equal(t, "chatgpt", modelConfig.Provider)
	assert.Equal(t, DefaultModels["chatgpt"], modelConfig.Model)
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

func TestPickDMRModel(t *testing.T) {
	t.Parallel()

	lister := func(models []string, err error) DMRModelLister {
		return func(context.Context) ([]string, error) { return models, err }
	}

	tests := []struct {
		name          string
		lister        DMRModelLister
		expectedModel string
		expectedLocal bool
	}{
		{"nil lister keeps default, not local", nil, "ai/qwen3", false},
		{"default pulled is local", lister([]string{"ai/qwen3"}, nil), "ai/qwen3", true},
		{"repo match under different tag is local", lister([]string{"ai/qwen3:Q4_K_M"}, nil), "ai/qwen3:Q4_K_M", true},
		{"first chat-capable installed is local", lister([]string{"qwen3:4B-UD-Q4_K_XL"}, nil), "qwen3:4B-UD-Q4_K_XL", true},
		{"embedding-only keeps default, not local", lister([]string{"ai/embeddinggemma"}, nil), "ai/qwen3", false},
		{"discovery error keeps default, not local", lister(nil, errors.New("dmr down")), "ai/qwen3", false},
		{"empty list keeps default, not local", lister([]string{}, nil), "ai/qwen3", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			model, foundLocal := PickDMRModel(t.Context(), "ai/qwen3", tt.lister)
			assert.Equal(t, tt.expectedModel, model)
			assert.Equal(t, tt.expectedLocal, foundLocal)
		})
	}
}

func TestPreferLocalDMRModels(t *testing.T) {
	t.Parallel()

	lister := func(models []string, err error) DMRModelLister {
		return func(context.Context) ([]string, error) { return models, err }
	}

	t.Run("uses a locally-pulled model and reports no fallback", func(t *testing.T) {
		t.Parallel()
		cfg := &latest.Config{Models: map[string]latest.ModelConfig{
			"smart": {Provider: "dmr", Model: "ai/qwen3"},
		}}
		noLocal := PreferLocalDMRModels(t.Context(), cfg, map[string]bool{"smart": true}, lister([]string{"qwen3:4B-UD-Q4_K_XL"}, nil))

		assert.Equal(t, "qwen3:4B-UD-Q4_K_XL", cfg.Models["smart"].Model)
		assert.NotContains(t, noLocal, "smart")
	})

	t.Run("no usable local model leaves default and flags the selector", func(t *testing.T) {
		t.Parallel()
		cfg := &latest.Config{Models: map[string]latest.ModelConfig{
			"smart": {Provider: "dmr", Model: "ai/qwen3"},
		}}
		noLocal := PreferLocalDMRModels(t.Context(), cfg, map[string]bool{"smart": true}, lister([]string{}, nil))

		assert.Equal(t, "ai/qwen3", cfg.Models["smart"].Model)
		assert.True(t, noLocal["smart"])
	})

	t.Run("non-dmr resolved selector is untouched", func(t *testing.T) {
		t.Parallel()
		cfg := &latest.Config{Models: map[string]latest.ModelConfig{
			"smart": {Provider: "anthropic", Model: "claude-sonnet-4-6"},
		}}
		called := false
		noLocal := PreferLocalDMRModels(t.Context(), cfg, map[string]bool{"smart": true}, func(context.Context) ([]string, error) {
			called = true
			return nil, nil
		})

		assert.Equal(t, "claude-sonnet-4-6", cfg.Models["smart"].Model)
		assert.Empty(t, noLocal)
		assert.False(t, called, "lister must not run for a non-dmr selection")
	})
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
		assert.Contains(t, msg, "docker agent setup")
		assert.Contains(t, msg, "docker model pull")
		assert.Contains(t, msg, "ANTHROPIC_API_KEY")
		assert.Contains(t, msg, environment.ModelSetupDocsURL)
		assert.NotContains(t, msg, "Could not initialize")
	})

	t.Run("with cause is surfaced and unwrappable", func(t *testing.T) {
		t.Parallel()

		cause := errors.New("model pull declined by user")
		err := &AutoModelFallbackError{Cause: cause}
		assert.Contains(t, err.Error(), "model pull declined by user")
		assert.ErrorIs(t, err, cause)
	})

	t.Run("pull-failure cause is summarized, not duplicated", func(t *testing.T) {
		t.Parallel()

		// A cause that carries its own multi-line "To fix this" guidance must be
		// reduced to its one-line summary so the two blocks don't stack.
		cause := &stubPullError{
			summary:    "failed to pull model ai/qwen3",
			fullDetail: "VERBOSE DETAIL: 416 Requested Range Not Satisfiable\nTo fix this: remove and re-pull",
		}
		err := &AutoModelFallbackError{Cause: cause}
		msg := err.Error()

		assert.Contains(t, msg, "Could not initialize the auto-selected model: failed to pull model ai/qwen3")
		assert.NotContains(t, msg, "VERBOSE DETAIL")
		// Exactly one remediation block (the one AutoModelFallbackError owns).
		assert.Equal(t, 1, strings.Count(msg, "To fix this"))
		assert.ErrorIs(t, err, cause)
	})
}

// stubPullError implements pullErrorSummarizer with a verbose Error() to prove
// the summary path avoids duplicating remediation guidance.
type stubPullError struct {
	summary    string
	fullDetail string
}

func (e *stubPullError) Error() string { return e.fullDetail }

func (e *stubPullError) ModelPullErrorSummary() string { return e.summary }

func TestProviderAPIKeyEnvVars(t *testing.T) {
	t.Parallel()

	vars := ProviderAPIKeyEnvVars()

	// Sorted and deduplicated for reproducibility.
	assert.True(t, slices.IsSorted(vars), "env vars must be sorted, got %v", vars)
	assert.Equal(t, slices.Compact(slices.Clone(vars)), vars, "env vars must be deduplicated")

	// The dedicated single-secret model API keys must be present.
	for _, name := range []string{
		"OPENAI_API_KEY",
		"ANTHROPIC_API_KEY",
		"GOOGLE_API_KEY",
		"MISTRAL_API_KEY",
		"OPENROUTER_API_KEY",
		"XAI_API_KEY",
		"NEBIUS_API_KEY",
	} {
		assert.Contains(t, vars, name)
	}

	// Non-secret detection/mode flags and multi-variable credential sets must
	// never be exposed as forwardable API keys.
	for _, name := range []string{
		"GOOGLE_GENAI_USE_VERTEXAI",
		"GEMINI_API_KEY",
		"AWS_ACCESS_KEY_ID",
		"AWS_PROFILE",
		"AWS_ROLE_ARN",
		"AWS_BEARER_TOKEN_BEDROCK",
	} {
		assert.NotContains(t, vars, name)
	}

	// Broad, general-purpose tokens must not be forwarded as model credentials.
	assert.NotContains(t, vars, "GITHUB_TOKEN")
	assert.NotContains(t, vars, "GH_TOKEN")

	// The ChatGPT OAuth access token is a subscription credential, not an
	// API key, and must never be forwarded into isolated environments.
	assert.NotContains(t, vars, chatgpt.TokenEnvVar)
}

func TestCloudProviderEnvVars(t *testing.T) {
	t.Parallel()

	providers := CloudProviderEnvVars()

	// Must mirror the auto-selection table: same providers, same priority
	// order, same detection variables.
	assert.Len(t, providers, len(cloudProviders))
	for i, p := range providers {
		assert.Equal(t, cloudProviders[i].name, p.Provider)
		assert.Equal(t, cloudProviders[i].envVars, p.EnvVars)
	}

	// Mutating the returned slices must not corrupt the package table.
	providers[0].EnvVars[0] = "MUTATED"
	assert.NotEqual(t, "MUTATED", cloudProviders[0].envVars[0])

	copilotIdx := slices.IndexFunc(providers, func(p ProviderEnvVars) bool {
		return p.Provider == "github-copilot"
	})
	require.GreaterOrEqual(t, copilotIdx, 0)
	assert.Equal(t, []string{"GITHUB_TOKEN", "GH_TOKEN"}, providers[copilotIdx].EnvVars)
}
