package provider

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/anthropic"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/bedrock"
	"github.com/docker/docker-agent/pkg/model/provider/dmr"
	"github.com/docker/docker-agent/pkg/model/provider/gemini"
	"github.com/docker/docker-agent/pkg/model/provider/openai"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/model/provider/rulebased"
	"github.com/docker/docker-agent/pkg/rag/types"
	"github.com/docker/docker-agent/pkg/tools"
)

// Alias defines the configuration for a provider alias
type Alias struct {
	APIType     string // The actual API type to use (openai, anthropic, etc.)
	BaseURL     string // Default base URL for the provider
	TokenEnvVar string // Environment variable name for the API token
}

// CoreProviders lists all natively implemented provider types.
// These are the provider types that have direct implementations (not aliases).
var CoreProviders = []string{
	"openai",
	"anthropic",
	"google",
	"dmr",
	"amazon-bedrock",
}

// AllProviders returns all known provider names (core providers + aliases),
// sorted for deterministic output.
func AllProviders() []string {
	providers := slices.Concat(CoreProviders, slices.Collect(maps.Keys(Aliases)))
	slices.Sort(providers)
	return providers
}

// IsKnownProvider returns true if the provider name is a core provider or an alias.
func IsKnownProvider(name string) bool {
	if slices.Contains(CoreProviders, strings.ToLower(name)) {
		return true
	}
	_, exists := Aliases[strings.ToLower(name)]
	return exists
}

// CatalogProviders returns the list of provider names that should be shown in the model catalog.
// This includes core providers and aliases that have a defined BaseURL (self-contained endpoints).
// Aliases without a BaseURL (like azure) require user configuration and are excluded.
func CatalogProviders() []string {
	providers := make([]string, 0, len(CoreProviders)+len(Aliases))

	// Add all core providers
	providers = append(providers, CoreProviders...)

	// Add aliases that have a defined BaseURL (they work out of the box)
	for name, alias := range Aliases {
		if alias.BaseURL != "" {
			providers = append(providers, name)
		}
	}

	return providers
}

// IsCatalogProvider returns true if the provider name is valid for the model catalog.
func IsCatalogProvider(name string) bool {
	// Check core providers
	if slices.Contains(CoreProviders, name) {
		return true
	}
	// Check aliases with BaseURL
	if alias, exists := Aliases[name]; exists && alias.BaseURL != "" {
		return true
	}
	return false
}

// Aliases maps provider names to their corresponding configurations
var Aliases = map[string]Alias{
	"requesty": {
		APIType:     "openai",
		BaseURL:     "https://router.requesty.ai/v1",
		TokenEnvVar: "REQUESTY_API_KEY",
	},
	"azure": {
		APIType:     "openai",
		TokenEnvVar: "AZURE_API_KEY",
	},
	"xai": {
		APIType:     "openai",
		BaseURL:     "https://api.x.ai/v1",
		TokenEnvVar: "XAI_API_KEY",
	},
	"nebius": {
		APIType:     "openai",
		BaseURL:     "https://api.studio.nebius.com/v1",
		TokenEnvVar: "NEBIUS_API_KEY",
	},
	"mistral": {
		APIType:     "openai",
		BaseURL:     "https://api.mistral.ai/v1",
		TokenEnvVar: "MISTRAL_API_KEY",
	},
	"ollama": {
		APIType: "openai",
		BaseURL: "http://localhost:11434/v1",
	},
	"minimax": {
		APIType:     "openai",
		BaseURL:     "https://api.minimax.io/v1",
		TokenEnvVar: "MINIMAX_API_KEY",
	},
}

// Provider defines the interface for model providers
type Provider interface {
	// ID returns the model provider ID
	ID() string
	// CreateChatCompletionStream creates a streaming chat completion request
	// It returns a stream that can be iterated over to get completion chunks
	CreateChatCompletionStream(
		ctx context.Context,
		messages []chat.Message,
		tools []tools.Tool,
	) (chat.MessageStream, error)
	// BaseConfig returns the base configuration of this provider
	BaseConfig() base.Config
}

// EmbeddingProvider defines the interface for providers that support embeddings.
type EmbeddingProvider interface {
	Provider
	// CreateEmbedding generates an embedding vector for the given text with usage tracking.
	CreateEmbedding(ctx context.Context, text string) (*base.EmbeddingResult, error)
}

// BatchEmbeddingProvider defines the interface for providers that support batch embeddings.
type BatchEmbeddingProvider interface {
	EmbeddingProvider
	// CreateBatchEmbedding generates embedding vectors for multiple texts with usage tracking.
	// Returns embeddings in the same order as input texts.
	CreateBatchEmbedding(ctx context.Context, texts []string) (*base.BatchEmbeddingResult, error)
}

// RerankingProvider defines the interface for providers that support reranking.
// Reranking models score query-document pairs to assess relevance.
type RerankingProvider interface {
	Provider
	// Rerank scores documents by relevance to the query.
	// Returns relevance scores in the same order as input documents.
	// Scores are typically in [0, 1] range where higher means more relevant.
	// criteria: Optional domain-specific guidance for relevance scoring (appended to base prompt)
	// documents: Array of types.Document with content and metadata
	Rerank(ctx context.Context, query string, documents []types.Document, criteria string) ([]float64, error)
}

// New creates a new provider from a model config.
// This is a convenience wrapper for NewWithModels with no models map.
func New(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
	return NewWithModels(ctx, cfg, nil, env, opts...)
}

// NewWithModels creates a new provider from a model config with access to the full models map.
// The models map is used to resolve model references in routing rules.
func NewWithModels(ctx context.Context, cfg *latest.ModelConfig, models map[string]latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
	slog.Debug("Creating model provider", "type", cfg.Provider, "model", cfg.Model)

	// Check if this model has routing rules - if so, create a rule-based router
	if len(cfg.Routing) > 0 {
		return createRuleBasedRouter(ctx, cfg, models, env, opts...)
	}

	return createDirectProvider(ctx, cfg, env, opts...)
}

// createRuleBasedRouter creates a rule-based routing provider.
func createRuleBasedRouter(ctx context.Context, cfg *latest.ModelConfig, models map[string]latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
	// Create a provider factory that can resolve model references
	factory := func(ctx context.Context, modelSpec string, models map[string]latest.ModelConfig, env environment.Provider, factoryOpts ...options.Opt) (rulebased.Provider, error) {
		// Check if modelSpec is a reference to a model in the models map
		if modelCfg, exists := models[modelSpec]; exists {
			// Prevent infinite recursion - referenced models cannot have routing rules
			if len(modelCfg.Routing) > 0 {
				return nil, fmt.Errorf("model %q has routing rules and cannot be used as a routing target", modelSpec)
			}
			p, err := createDirectProvider(ctx, &modelCfg, env, factoryOpts...)
			if err != nil {
				return nil, err
			}
			return p, nil
		}

		// Otherwise, treat as an inline model spec (e.g., "openai/gpt-4o")
		inlineCfg, parseErr := latest.ParseModelRef(modelSpec)
		if parseErr != nil {
			return nil, fmt.Errorf("invalid model spec %q: expected 'provider/model' format or a model reference", modelSpec)
		}
		p, err := createDirectProvider(ctx, &inlineCfg, env, factoryOpts...)
		if err != nil {
			return nil, err
		}
		return p, nil
	}

	return rulebased.NewClient(ctx, cfg, models, env, factory, opts...)
}

// createDirectProvider creates a provider without routing (direct model access).
func createDirectProvider(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
	var globalOptions options.ModelOptions
	for _, opt := range opts {
		opt(&globalOptions)
	}

	// Apply defaults from custom providers (from config) or built-in aliases
	enhancedCfg := applyProviderDefaults(cfg, globalOptions.Providers())
	if thinking := globalOptions.Thinking(); thinking != nil && !*thinking {
		enhancedCfg.ThinkingBudget = nil

		// with thinking explicitly disabled, also remove the interleaved_thinking provider option
		if enhancedCfg.ProviderOpts != nil {
			// Copy to avoid mutating shared ProviderOpts in the original config
			optsCopy := make(map[string]any, len(enhancedCfg.ProviderOpts))
			maps.Copy(optsCopy, enhancedCfg.ProviderOpts)
			delete(optsCopy, "interleaved_thinking")
			enhancedCfg.ProviderOpts = optsCopy
		}
	}

	// Apply overrides (e.g., disable thinking if requested by session)
	enhancedCfg = applyOverrides(enhancedCfg, &globalOptions)

	// Resolve the provider type with priority:
	// 1. cfg.ProviderOpts["api_type"] (from custom provider or model override)
	// 2. built-in alias APIType
	// 3. provider name itself
	providerType := resolveProviderTypeFromConfig(enhancedCfg)

	switch providerType {
	case "openai", "openai_chatcompletions", "openai_responses":
		return openai.NewClient(ctx, enhancedCfg, env, opts...)

	case "anthropic":
		return anthropic.NewClient(ctx, enhancedCfg, env, opts...)

	case "google":
		return gemini.NewClient(ctx, enhancedCfg, env, opts...)

	case "dmr":
		return dmr.NewClient(ctx, enhancedCfg, opts...)

	case "amazon-bedrock":
		return bedrock.NewClient(ctx, enhancedCfg, env, opts...)

	default:
		slog.Error("Unknown provider type", "type", providerType)
		return nil, fmt.Errorf("unknown provider type: %s", providerType)
	}
}

// resolveProviderTypeFromConfig determines the provider type to use based on config.
// Priority:
// 1. cfg.ProviderOpts["api_type"] (from custom provider or model-level override)
// 2. built-in alias APIType (e.g., "mistral" -> "openai")
// 3. provider name itself (e.g., "openai", "anthropic")
func resolveProviderTypeFromConfig(cfg *latest.ModelConfig) string {
	// Check for api_type in ProviderOpts (set by custom providers or model override)
	if cfg.ProviderOpts != nil {
		if apiType, ok := cfg.ProviderOpts["api_type"].(string); ok && apiType != "" {
			slog.Debug("Using api_type from provider config",
				"provider", cfg.Provider,
				"model", cfg.Model,
				"api_type", apiType,
				"base_url", cfg.BaseURL,
			)
			return apiType
		}
	}

	// Check built-in alias
	if alias, exists := Aliases[cfg.Provider]; exists && alias.APIType != "" {
		return alias.APIType
	}

	// Fall back to provider name
	return cfg.Provider
}

// applyProviderDefaults applies default configuration from custom providers or built-in aliases.
// Custom providers (from config) take precedence over built-in aliases.
// This sets default base URLs, token keys, api_type, and model-specific defaults (like thinking budget).
func applyProviderDefaults(cfg *latest.ModelConfig, customProviders map[string]latest.ProviderConfig) *latest.ModelConfig {
	// Create a copy to avoid modifying the original
	enhancedCfg := *cfg

	if customProviders != nil {
		if providerCfg, exists := customProviders[cfg.Provider]; exists {
			slog.Debug("Applying custom provider defaults",
				"provider", cfg.Provider,
				"model", cfg.Model,
				"base_url", providerCfg.BaseURL,
			)

			if enhancedCfg.BaseURL == "" && providerCfg.BaseURL != "" {
				enhancedCfg.BaseURL = providerCfg.BaseURL
			}
			if enhancedCfg.TokenKey == "" && providerCfg.TokenKey != "" {
				enhancedCfg.TokenKey = providerCfg.TokenKey
			}

			// Set api_type in ProviderOpts if not already set
			if enhancedCfg.ProviderOpts == nil {
				enhancedCfg.ProviderOpts = make(map[string]any)
			}
			if _, has := enhancedCfg.ProviderOpts["api_type"]; !has {
				apiType := providerCfg.APIType
				if apiType == "" {
					apiType = "openai_chatcompletions"
				}
				enhancedCfg.ProviderOpts["api_type"] = apiType
			}

			// Copy custom headers from provider config if not already set in provider_opts
			// Deep-copy the map to avoid mutating the shared provider config
			if _, hasHeaders := enhancedCfg.ProviderOpts["headers"]; !hasHeaders {
				if len(providerCfg.Headers) > 0 {
					headersCopy := make(map[string]string, len(providerCfg.Headers))
					for k, v := range providerCfg.Headers {
						headersCopy[k] = v
					}
					enhancedCfg.ProviderOpts["headers"] = headersCopy
				}
			}

			// Merge model-level headers into provider_opts headers (model-level takes precedence)
			if len(enhancedCfg.Headers) > 0 {
				existing, _ := enhancedCfg.ProviderOpts["headers"].(map[string]string)
				if existing == nil {
					existing = make(map[string]string)
				}
				for k, v := range enhancedCfg.Headers {
					existing[k] = v
				}
				enhancedCfg.ProviderOpts["headers"] = existing
			}

			applyModelDefaults(&enhancedCfg)
			return &enhancedCfg
		}
	}

	if alias, exists := Aliases[cfg.Provider]; exists {
		// Set default base URL if not already specified
		if enhancedCfg.BaseURL == "" && alias.BaseURL != "" {
			enhancedCfg.BaseURL = alias.BaseURL
		}

		// Set default token key if not already specified
		if enhancedCfg.TokenKey == "" && alias.TokenEnvVar != "" {
			enhancedCfg.TokenKey = alias.TokenEnvVar
		}
	}

	// Merge model-level headers into provider_opts for non-custom providers too
	if len(enhancedCfg.Headers) > 0 {
		if enhancedCfg.ProviderOpts == nil {
			enhancedCfg.ProviderOpts = make(map[string]any)
		}
		existing, _ := enhancedCfg.ProviderOpts["headers"].(map[string]string)
		if existing == nil {
			existing = make(map[string]string)
		}
		for k, v := range enhancedCfg.Headers {
			existing[k] = v
		}
		enhancedCfg.ProviderOpts["headers"] = existing
	}

	// Apply model-specific defaults (e.g., thinking budget for Claude/GPT models)
	applyModelDefaults(&enhancedCfg)
	return &enhancedCfg
}

// applyOverrides applies session-level or request-level overrides to the configuration.
// This is called AFTER defaults are applied, allowing overrides to clear or modify default values.
func applyOverrides(cfg *latest.ModelConfig, opts *options.ModelOptions) *latest.ModelConfig {
	if opts == nil {
		return cfg
	}

	// Create a copy to avoid modifying the original
	enhancedCfg := *cfg

	t := opts.Thinking()
	if t == nil {
		return &enhancedCfg
	}

	// If thinking is explicitly disabled (e.g., via /think command), clear thinking configuration
	if !*t {
		enhancedCfg.ThinkingBudget = nil
		if enhancedCfg.ProviderOpts != nil {
			delete(enhancedCfg.ProviderOpts, "interleaved_thinking")
		}
		slog.Debug("Override: thinking disabled - cleared thinking configuration",
			"provider", cfg.Provider,
			"model", cfg.Model,
		)
		return &enhancedCfg
	}

	// If thinking is explicitly enabled (e.g., via /think command), ensure thinking is configured.
	// This handles two cases:
	// 1. ThinkingBudget is nil (not configured) - apply defaults to enable thinking
	// 2. ThinkingBudget is explicitly disabled (Tokens == 0 or Effort == "none") - clear and re-apply defaults
	// This allows /think to enable thinking with provider defaults even when config had thinking_budget: 0
	if enhancedCfg.ThinkingBudget == nil || enhancedCfg.ThinkingBudget.IsDisabled() {
		enhancedCfg.ThinkingBudget = nil
		applyModelDefaults(&enhancedCfg)
		slog.Debug("Override: thinking enabled - applied default thinking configuration",
			"provider", cfg.Provider,
			"model", cfg.Model,
			"thinking_budget", enhancedCfg.ThinkingBudget,
		)
	}

	return &enhancedCfg
}

// applyModelDefaults applies provider-specific default values for model configuration.
// These defaults are applied only if the user hasn't explicitly set the values.
//
// NOTE: max_tokens is NOT set here because:
// 1. Different providers read it differently (ModelConfig vs ModelOptions)
// 2. Runtime can do modelsdev lookups for model-specific limits
// 3. Providers have their own fallbacks (e.g., Anthropic defaults to 8192)
// max_tokens defaults are handled in teamloader and runtime/model_switcher via options.
//
// Config-level defaults (set here):
// - OpenAI: thinking_budget = "medium"
// - Anthropic: thinking_budget = 8192, interleaved_thinking = true
// - Google: Gemini 2.5 → thinking_budget = -1 (dynamic), Gemini 3 Pro → "high", Gemini 3 Flash → "medium"
// - Amazon Bedrock (Claude models only): thinking_budget = 8192, interleaved_thinking = true
func applyModelDefaults(cfg *latest.ModelConfig) {
	// If thinking is explicitly disabled (thinking_budget: 0 or thinking_budget: none),
	// set ThinkingBudget to nil to completely disable thinking.
	// This ensures no thinking config is sent to the provider.
	if cfg.ThinkingBudget.IsDisabled() {
		cfg.ThinkingBudget = nil
		slog.Debug("Thinking explicitly disabled via thinking_budget: 0 or none",
			"provider", cfg.Provider,
			"model", cfg.Model,
		)
		return // Don't apply any provider defaults for thinking
	}

	// Resolve the actual provider type (handling aliases like mistral -> openai)
	providerType := cfg.Provider
	if alias, exists := Aliases[cfg.Provider]; exists && alias.APIType != "" {
		providerType = alias.APIType
	}
	// Also check for api_type override in ProviderOpts
	if cfg.ProviderOpts != nil {
		if apiType, ok := cfg.ProviderOpts["api_type"].(string); ok && apiType != "" {
			providerType = apiType
		}
	}

	switch providerType {
	case "openai", "openai_chatcompletions", "openai_responses":
		applyOpenAIDefaults(cfg)
	case "anthropic":
		applyAnthropicDefaults(cfg)
	case "google":
		applyGoogleDefaults(cfg)
	case "amazon-bedrock":
		applyBedrockDefaults(cfg)
	}
}

// applyOpenAIDefaults applies default configuration for OpenAI models.
func applyOpenAIDefaults(cfg *latest.ModelConfig) {
	// Default thinking_budget to "medium" if not set
	if cfg.ThinkingBudget == nil {
		cfg.ThinkingBudget = &latest.ThinkingBudget{Effort: "medium"}
		slog.Debug("Applied default thinking_budget for OpenAI",
			"provider", cfg.Provider,
			"model", cfg.Model,
			"thinking_budget", "medium",
		)
	}
}

// applyAnthropicDefaults applies default configuration for Anthropic models.
func applyAnthropicDefaults(cfg *latest.ModelConfig) {
	// Default thinking_budget to 8192 tokens if not set
	if cfg.ThinkingBudget == nil {
		cfg.ThinkingBudget = &latest.ThinkingBudget{Tokens: 8192}
		slog.Debug("Applied default thinking_budget for Anthropic",
			"provider", cfg.Provider,
			"model", cfg.Model,
			"thinking_budget", 8192,
		)
	}

	// Default interleaved_thinking to true if not set
	if cfg.ProviderOpts == nil {
		cfg.ProviderOpts = make(map[string]any)
	}
	if _, has := cfg.ProviderOpts["interleaved_thinking"]; !has {
		cfg.ProviderOpts["interleaved_thinking"] = true
		slog.Debug("Applied default interleaved_thinking for Anthropic",
			"provider", cfg.Provider,
			"model", cfg.Model,
			"interleaved_thinking", true,
		)
	}
}

// applyGoogleDefaults applies default configuration for Google Gemini models.
// - Gemini 2.5 models: thinking_budget = -1 (dynamic thinking)
// - Gemini 3+ Pro models: thinking_budget effort = "high"
// - Gemini 3+ Flash models: thinking_budget effort = "medium"
func applyGoogleDefaults(cfg *latest.ModelConfig) {
	if cfg.ThinkingBudget != nil {
		return // User explicitly set thinking_budget
	}

	model := strings.ToLower(cfg.Model)

	switch {
	case strings.HasPrefix(model, "gemini-2.5-"):
		// Gemini 2.5 models use token-based thinking budget (-1 = dynamic)
		cfg.ThinkingBudget = &latest.ThinkingBudget{Tokens: -1}
		slog.Debug("Applied default thinking_budget for Google Gemini 2.5",
			"provider", cfg.Provider,
			"model", cfg.Model,
			"thinking_budget", -1,
		)
	case isGeminiProModel(model):
		// Gemini 3+ Pro models use level-based thinking (high)
		cfg.ThinkingBudget = &latest.ThinkingBudget{Effort: "high"}
		slog.Debug("Applied default thinking_budget for Google Gemini 3+ Pro",
			"provider", cfg.Provider,
			"model", cfg.Model,
			"thinking_budget", "high",
		)
	case isGeminiFlashModel(model):
		// Gemini 3+ Flash models use level-based thinking (medium)
		cfg.ThinkingBudget = &latest.ThinkingBudget{Effort: "medium"}
		slog.Debug("Applied default thinking_budget for Google Gemini 3+ Flash",
			"provider", cfg.Provider,
			"model", cfg.Model,
			"thinking_budget", "medium",
		)
	}
	// For other Gemini models (e.g., gemini-2.0-*), leave unchanged
}

// gemini3Family extracts the model family (e.g. "pro", "flash") from a
// Gemini 3+ model name, or returns "" if the model is not Gemini 3+.
// It handles both "gemini-3-<family>" and "gemini-3.X-<family>" patterns.
//
// Examples:
//
//	gemini3Family("gemini-3-pro")              → "pro"
//	gemini3Family("gemini-3.1-flash-preview")  → "flash-preview"
//	gemini3Family("gemini-2.5-flash")          → ""
func gemini3Family(model string) string {
	if !strings.HasPrefix(model, "gemini-3") {
		return ""
	}
	rest := model[len("gemini-3"):]
	if rest == "" {
		return ""
	}
	// Accept "gemini-3-..." or "gemini-3.X-..." (e.g. gemini-3.1-pro-preview)
	switch rest[0] {
	case '-':
		return rest[1:] // "gemini-3-pro" → "pro"
	case '.':
		if _, family, ok := strings.Cut(rest, "-"); ok {
			return family // "gemini-3.1-pro-preview" → "pro-preview"
		}
	}
	return ""
}

func isGeminiProModel(model string) bool {
	return strings.HasPrefix(gemini3Family(model), "pro")
}

func isGeminiFlashModel(model string) bool {
	return strings.HasPrefix(gemini3Family(model), "flash")
}

// applyBedrockDefaults applies default configuration for Amazon Bedrock models.
// Only applies to Claude models (anthropic.claude-* or global.anthropic.claude-*).
func applyBedrockDefaults(cfg *latest.ModelConfig) {
	// Only apply defaults for Claude models on Bedrock
	if !isBedrockClaudeModel(cfg.Model) {
		return
	}

	// Default thinking_budget to 8192 tokens if not set
	if cfg.ThinkingBudget == nil {
		cfg.ThinkingBudget = &latest.ThinkingBudget{Tokens: 8192}
		slog.Debug("Applied default thinking_budget for Bedrock Claude",
			"provider", cfg.Provider,
			"model", cfg.Model,
			"thinking_budget", 8192,
		)
	}

	// Default interleaved_thinking to true if not set
	if cfg.ProviderOpts == nil {
		cfg.ProviderOpts = make(map[string]any)
	}
	if _, has := cfg.ProviderOpts["interleaved_thinking"]; !has {
		cfg.ProviderOpts["interleaved_thinking"] = true
		slog.Debug("Applied default interleaved_thinking for Bedrock Claude",
			"provider", cfg.Provider,
			"model", cfg.Model,
			"interleaved_thinking", true,
		)
	}
}

// isBedrockClaudeModel returns true if the model ID is a Claude model on Bedrock.
// Claude model IDs on Bedrock start with "anthropic.claude-" or "global.anthropic.claude-".
func isBedrockClaudeModel(model string) bool {
	m := strings.ToLower(model)
	return strings.HasPrefix(m, "anthropic.claude-") || strings.HasPrefix(m, "global.anthropic.claude-")
}
