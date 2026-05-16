package config

import (
	"context"
	"fmt"
	"strings"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

// providerConfig defines a cloud provider and how to detect/describe its API keys.
type providerConfig struct {
	name    string   // provider name (e.g., "anthropic")
	envVars []string // env vars to check - provider is available if ANY is set
	hint    string   // description for error messages
}

// cloudProviders defines the available cloud providers in priority order.
// The first provider with a configured API key will be selected by AutoModelConfig.
// DMR is always appended as the final fallback (not listed here).
var cloudProviders = []providerConfig{
	{"anthropic", []string{"ANTHROPIC_API_KEY"}, "ANTHROPIC_API_KEY"},
	{"openai", []string{"OPENAI_API_KEY"}, "OPENAI_API_KEY"},
	{"google", []string{
		"GOOGLE_API_KEY",
		"GEMINI_API_KEY",
		"GOOGLE_GENAI_USE_VERTEXAI",
	}, "GOOGLE_API_KEY (or GEMINI_API_KEY, GOOGLE_GENAI_USE_VERTEXAI)"},
	{"mistral", []string{"MISTRAL_API_KEY"}, "MISTRAL_API_KEY"},
	{"amazon-bedrock", []string{
		"AWS_BEARER_TOKEN_BEDROCK",
		"AWS_ACCESS_KEY_ID",
		"AWS_PROFILE",
		"AWS_ROLE_ARN",
	}, "AWS_ACCESS_KEY_ID (or AWS_PROFILE, AWS_ROLE_ARN, AWS_BEARER_TOKEN_BEDROCK)"},
}

// AutoModelFallbackError is returned when auto model selection fails because
// no providers are available (no API keys configured and DMR not installed).
type AutoModelFallbackError struct{}

func (e *AutoModelFallbackError) Error() string {
	var hints []string
	for _, p := range cloudProviders {
		hints = append(hints, fmt.Sprintf("    - %s: %s", p.name, p.hint))
	}

	return "No model providers available.\n\nTo fix this, you can:\n" +
		"  - Install Docker Model Runner: https://docs.docker.com/ai/model-runner/get-started/\n" +
		"  - Configure an API key for a cloud provider:\n" +
		strings.Join(hints, "\n")
}

var DefaultModels = map[string]string{
	"openai":         "gpt-5-mini",
	"anthropic":      "claude-sonnet-4-6",
	"google":         "gemini-2.5-flash",
	"dmr":            "ai/qwen3:latest",
	"mistral":        "mistral-small-latest",
	"amazon-bedrock": "global.anthropic.claude-sonnet-4-5-20250929-v1:0",
}

func AvailableProviders(ctx context.Context, modelsGateway string, env environment.Provider) []string {
	if modelsGateway != "" {
		// Default to anthropic when using a gateway
		return []string{"anthropic"}
	}

	var providers []string

	for _, p := range cloudProviders {
		for _, envVar := range p.envVars {
			if key, _ := env.Get(ctx, envVar); key != "" {
				providers = append(providers, p.name)
				break // found one, no need to check other env vars for this provider
			}
		}
	}

	// DMR is always the final fallback
	providers = append(providers, "dmr")

	return providers
}

func AutoModelConfig(ctx context.Context, modelsGateway string, env environment.Provider, defaultModel *latest.ModelConfig) latest.ModelConfig {
	// If user specified a default model config, use it (with defaults for unset fields)
	if defaultModel != nil && defaultModel.Provider != "" && defaultModel.Model != "" {
		result := *defaultModel
		if result.MaxTokens == nil {
			result.MaxTokens = PreferredMaxTokens(result.Provider)
		}
		return result
	}

	availableProviders := AvailableProviders(ctx, modelsGateway, env)
	firstAvailable := availableProviders[0]

	return latest.ModelConfig{
		Provider:  firstAvailable,
		Model:     DefaultModels[firstAvailable],
		MaxTokens: PreferredMaxTokens(firstAvailable),
	}
}

func PreferredMaxTokens(provider string) *int64 {
	var mt int64 = 32000
	if provider == "dmr" {
		mt = 16000
	}
	return &mt
}

// AutoEmbeddingModelConfigs returns the ordered list of embedding-capable models
// to try when a RAG strategy uses `model: auto` for embeddings.
//
// The priority is:
//  1. OpenAI -> text-embedding-3-small model
//  2. DMR -> Google's embeddinggemma model (via Docker Model Runner)
func AutoEmbeddingModelConfigs() []latest.ModelConfig {
	return []latest.ModelConfig{
		{
			Provider: "openai",
			Model:    "text-embedding-3-small",
		},
		{
			Provider: "dmr",
			Model:    "ai/embeddinggemma",
		},
	}
}
