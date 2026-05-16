package provider

import (
	"cmp"
	"log/slog"
	"maps"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/modelinfo"
)

// ---------------------------------------------------------------------------
// Provider-type resolution
// ---------------------------------------------------------------------------

// resolveProviderType determines the effective API type for a config.
// Priority: ProviderOpts["api_type"] > built-in alias > provider name.
// Reading from a nil ProviderOpts map is safe and yields the zero value.
func resolveProviderType(cfg *latest.ModelConfig) string {
	if apiType, ok := cfg.ProviderOpts["api_type"].(string); ok && apiType != "" {
		return apiType
	}
	if alias, exists := LookupAlias(cfg.Provider); exists && alias.APIType != "" {
		return alias.APIType
	}
	return cfg.Provider
}

// resolveEffectiveProvider returns the effective provider type for a ProviderConfig.
// If Provider is explicitly set, returns that. Otherwise returns "openai" (backward compat).
func resolveEffectiveProvider(cfg latest.ProviderConfig) string {
	return cmp.Or(cfg.Provider, "openai")
}

// isOpenAICompatibleProvider returns true if the provider type uses the OpenAI API protocol.
func isOpenAICompatibleProvider(providerType string) bool {
	switch providerType {
	case "openai", "openai_chatcompletions", "openai_responses":
		return true
	}
	// Otherwise, the type is OpenAI-compatible iff it's an alias that maps to OpenAI.
	alias, exists := LookupAlias(providerType)
	return exists && alias.APIType == "openai"
}

// ---------------------------------------------------------------------------
// Provider defaults
// ---------------------------------------------------------------------------

// applyProviderDefaults applies default configuration from custom providers or built-in aliases.
// Custom providers (from config) take precedence over built-in aliases.
// This sets default base URLs, token keys, api_type, and model-specific defaults (like thinking budget).
//
// The returned config is a deep-enough copy: the caller's ModelConfig, ProviderOpts map,
// and ThinkingBudget/TaskBudget pointers are never mutated.
func applyProviderDefaults(cfg *latest.ModelConfig, customProviders map[string]latest.ProviderConfig) *latest.ModelConfig {
	// Create a copy to avoid modifying the original.
	// cloneModelConfig also deep-copies ProviderOpts so writes are safe.
	enhancedCfg := cloneModelConfig(cfg)

	if providerCfg, exists := customProviders[cfg.Provider]; exists {
		slog.Debug("Applying custom provider defaults",
			"provider", cfg.Provider,
			"model", cfg.Model,
			"base_url", providerCfg.BaseURL,
		)
		mergeFromProviderConfig(enhancedCfg, providerCfg)
		applyModelDefaults(enhancedCfg)
		return enhancedCfg
	}

	if alias, exists := LookupAlias(cfg.Provider); exists {
		applyAliasFallbacks(enhancedCfg, alias)
	}

	// Apply model-specific defaults (e.g., thinking budget for Claude/GPT models)
	applyModelDefaults(enhancedCfg)
	return enhancedCfg
}

// mergeFromProviderConfig merges defaults from a custom ProviderConfig into a
// ModelConfig. Model-level fields always take precedence; provider-level fields
// only fill in unset (nil/empty) fields. The destination ProviderOpts map is
// assumed to be safe to mutate (cloneModelConfig copies it).
func mergeFromProviderConfig(dst *latest.ModelConfig, src latest.ProviderConfig) {
	// Apply the underlying provider type if set on the provider config.
	// This allows the model to inherit the real provider type (e.g., "anthropic")
	// so that the correct API client is selected.
	if src.Provider != "" {
		dst.Provider = src.Provider
	}

	if dst.BaseURL == "" {
		dst.BaseURL = src.BaseURL
	}
	if dst.TokenKey == "" {
		dst.TokenKey = src.TokenKey
	}
	// Plumb the provider-level unload endpoint into ProviderOpts so the
	// provider implementation can pick it up at unload time without
	// needing a back-reference to the parent ProviderConfig.
	if src.UnloadAPI != "" {
		setProviderOptIfAbsent(dst, "unload_api", src.UnloadAPI)
	}
	setIfNil(&dst.Temperature, src.Temperature)
	setIfNil(&dst.MaxTokens, src.MaxTokens)
	setIfNil(&dst.TopP, src.TopP)
	setIfNil(&dst.FrequencyPenalty, src.FrequencyPenalty)
	setIfNil(&dst.PresencePenalty, src.PresencePenalty)
	setIfNil(&dst.ParallelToolCalls, src.ParallelToolCalls)
	setIfNil(&dst.TrackUsage, src.TrackUsage)
	setIfNil(&dst.ThinkingBudget, src.ThinkingBudget)
	setIfNil(&dst.TaskBudget, src.TaskBudget)
	// Inherit Auth from the provider config when the model does not
	// override it. Auth is a pointer so a model-level value (even a
	// stub like {type: workload_identity_federation}) wins as a whole;
	// we deliberately do not merge field-by-field across the levels.
	setIfNil(&dst.Auth, src.Auth)

	// Merge provider_opts from provider config (model opts take precedence).
	for k, v := range src.ProviderOpts {
		setProviderOptIfAbsent(dst, k, v)
	}

	// Set api_type in ProviderOpts if not already set.
	// Prefer the explicit APIType from the provider config; otherwise default to
	// openai_chatcompletions for OpenAI-compatible providers.
	switch {
	case src.APIType != "":
		setProviderOptIfAbsent(dst, "api_type", src.APIType)
	case isOpenAICompatibleProvider(resolveEffectiveProvider(src)):
		setProviderOptIfAbsent(dst, "api_type", "openai_chatcompletions")
	}
}

// applyAliasFallbacks applies BaseURL and TokenKey defaults from a built-in
// alias when the model config does not already specify them.
func applyAliasFallbacks(dst *latest.ModelConfig, alias Alias) {
	if dst.BaseURL == "" {
		dst.BaseURL = alias.BaseURL
	}
	if dst.TokenKey == "" {
		dst.TokenKey = alias.TokenEnvVar
	}
}

// setIfNil assigns src to *dst when *dst is nil. It centralises the repetitive
// "only fill in if unset" pattern used when merging provider-level defaults.
func setIfNil[T any](dst **T, src *T) {
	if *dst == nil && src != nil {
		*dst = src
	}
}

// setProviderOptIfAbsent stores key=value in cfg.ProviderOpts unless the key is
// already set. The map is created lazily.
func setProviderOptIfAbsent(cfg *latest.ModelConfig, key string, value any) {
	if cfg.ProviderOpts == nil {
		cfg.ProviderOpts = make(map[string]any)
	}
	if _, has := cfg.ProviderOpts[key]; !has {
		cfg.ProviderOpts[key] = value
	}
}

// cloneModelConfig returns a shallow copy of cfg with a deep copy of
// ProviderOpts so that callers can safely mutate the returned config's
// map and pointer fields without affecting the original.
func cloneModelConfig(cfg *latest.ModelConfig) *latest.ModelConfig {
	c := *cfg
	if cfg.ProviderOpts != nil {
		c.ProviderOpts = make(map[string]any, len(cfg.ProviderOpts))
		maps.Copy(c.ProviderOpts, cfg.ProviderOpts)
	}
	return &c
}

// ---------------------------------------------------------------------------
// Thinking defaults and overrides
// ---------------------------------------------------------------------------

// applyModelDefaults applies provider-specific default values for model configuration.
//
// Thinking defaults policy:
//   - thinking_budget: 0  or  thinking_budget: none  →  thinking is off (nil).
//   - thinking_budget explicitly set to a real value  →  kept as-is; interleaved_thinking
//     is auto-enabled for Anthropic/Bedrock-Claude.
//   - thinking_budget NOT set:
//   - Thinking-only models (OpenAI o-series) get "medium".
//   - All other models get no thinking.
//
// NOTE: max_tokens is NOT set here; see teamloader and runtime/model_switcher.
func applyModelDefaults(cfg *latest.ModelConfig) {
	// Explicitly disabled → normalise to nil so providers never see it.
	if cfg.ThinkingBudget.IsDisabled() {
		cfg.ThinkingBudget = nil
		slog.Debug("Thinking explicitly disabled",
			"provider", cfg.Provider, "model", cfg.Model)
		return
	}

	providerType := resolveProviderType(cfg)

	// User already set a real thinking_budget — just apply side-effects.
	if cfg.ThinkingBudget != nil {
		ensureInterleavedThinking(cfg, providerType)
		return
	}

	// No thinking_budget configured — only models that always reason get a default.
	switch providerType {
	case "openai", "openai_chatcompletions", "openai_responses":
		if modelinfo.AlwaysReasons(cfg.Model) {
			cfg.ThinkingBudget = &latest.ThinkingBudget{Effort: "medium"}
			slog.Debug("Applied default thinking for thinking-only OpenAI model",
				"provider", cfg.Provider, "model", cfg.Model)
		}
	}
}

// ensureInterleavedThinking sets interleaved_thinking=true in ProviderOpts
// for any Claude model, unless the user already set it.
//
// Anthropic's Claude API requires the `interleaved-thinking-2025-05-14` beta
// header to interleave tool use with extended thinking. The same goes for the
// Bedrock-hosted Claude models. We auto-enable it whenever a thinking budget
// is configured on a Claude model so users don't have to remember the flag.
func ensureInterleavedThinking(cfg *latest.ModelConfig, providerType string) {
	if !needsInterleavedThinking(providerType, cfg.Model) {
		return
	}
	if cfg.ProviderOpts == nil {
		cfg.ProviderOpts = make(map[string]any)
	}
	if _, has := cfg.ProviderOpts["interleaved_thinking"]; !has {
		cfg.ProviderOpts["interleaved_thinking"] = true
		slog.Debug("Auto-enabled interleaved_thinking",
			"provider", cfg.Provider, "model", cfg.Model)
	}
}

// needsInterleavedThinking reports whether a (provider, model) pair refers to
// a Claude model on a host that supports the interleaved-thinking beta.
func needsInterleavedThinking(providerType, model string) bool {
	switch providerType {
	case "anthropic":
		return true
	case "amazon-bedrock":
		return modelinfo.IsBedrockClaudeID(model)
	}
	return false
}
