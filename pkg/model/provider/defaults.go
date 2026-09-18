package provider

import (
	"cmp"
	"context"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelinfo"
)

// expandModelConfigEnv substitutes ${env.X} / ${X} references in the model
// config's value-bearing fields (model, base_url) against the runtime
// environment. createDirectProvider calls it on the cloned, defaults-applied
// config so every leaf provider (direct, routed, fallback, title) dials the
// resolved endpoint and sends the resolved model id. See issue #2261.
func expandModelConfigEnv(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider) error {
	return cfg.ExpandEnv(func(s string) (string, error) {
		return environment.Expand(ctx, s, env)
	})
}

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

// ResolveType returns the registry key a model config is served by once
// custom providers (providers: section), built-in aliases and an explicit
// api_type are applied — i.e. the factory [Registry.New] will look up for
// it. A routing model resolves to its fallback provider/model. The config is
// not mutated.
func ResolveType(cfg *latest.ModelConfig, customProviders map[string]latest.ProviderConfig) string {
	return resolveProviderType(applyProviderDefaults(cfg, customProviders))
}

// Adapt turns a provider package's concrete constructor (e.g.
// anthropic.NewClient, which returns *anthropic.Client) into a [Factory] so
// it can be registered without a hand-written closure:
//
//	provider.NewRegistry(map[string]provider.Factory{"anthropic": provider.Adapt(anthropic.NewClient)})
func Adapt[P Provider](newClient func(context.Context, *latest.ModelConfig, environment.Provider, ...options.Opt) (P, error)) Factory {
	return func(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
		p, err := newClient(ctx, cfg, env, opts...)
		if err != nil {
			return nil, err
		}
		return p, nil
	}
}

// Has reports whether a factory is registered for providerType, a key as
// returned by [ResolveType].
func (r *Registry) Has(providerType string) bool {
	if r == nil {
		return false
	}
	_, ok := r.factory(providerType)
	return ok
}

// factory looks up the factory for a resolved provider type. The OpenAI
// protocol variants ("openai_chatcompletions", "openai_responses") fall back
// to the "openai" factory, which serves both, so hand-picked registries only
// need to register "openai" to cover custom OpenAI-compatible providers.
func (r *Registry) factory(providerType string) (Factory, bool) {
	if f, ok := r.factories[providerType]; ok {
		return f, true
	}
	if strings.HasPrefix(providerType, "openai_") {
		f, ok := r.factories["openai"]
		return f, ok
	}
	return nil, false
}

// Types returns the registered provider types, sorted.
func (r *Registry) Types() []string {
	if r == nil {
		return nil
	}
	return slices.Sorted(maps.Keys(r.factories))
}

// resolveEffectiveProvider returns the effective provider type for a ProviderConfig.
// If Provider is explicitly set, returns that. Otherwise returns "openai" (backward compat).
func resolveEffectiveProvider(cfg latest.ProviderConfig) string {
	return cmp.Or(cfg.Provider, "openai")
}

// defaultOpenAIAPIType returns the api_type to default to for an
// OpenAI-compatible provider that did not specify one explicitly.
//
// Newer OpenAI models (gpt-4.1, the o-series, gpt-5 and Codex) are only
// served via the Responses API; the legacy Chat Completions endpoint rejects
// them with a 400 ("unsupported_api_for_model"). Built-in providers (openai,
// github-copilot) get this routing for free via the client's per-request
// auto-detection, but custom providers in the providers: section bypass that
// path because an explicit api_type pins the endpoint. Defaulting on the same
// modelinfo.SupportsResponsesAPI predicate keeps both paths consistent so a
// provider pointed at the OpenAI/Copilot API works without a manual
// api_type override. See https://github.com/docker/docker-agent/issues/2303.
func defaultOpenAIAPIType(model string) string {
	if modelinfo.SupportsResponsesAPI(model) {
		return "openai_responses"
	}
	return "openai_chatcompletions"
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
	// Prefer the explicit APIType from the provider config; otherwise pick a
	// default for OpenAI-compatible providers based on the model.
	switch {
	case src.APIType != "":
		setProviderOptIfAbsent(dst, "api_type", src.APIType)
	case isOpenAICompatibleProvider(resolveEffectiveProvider(src)):
		setProviderOptIfAbsent(dst, "api_type", defaultOpenAIAPIType(dst.Model))
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
//   - thinking_budget: 0  →  thinking is off (nil): the model's own default
//     applies since most providers have no real "off" switch.
//   - thinking_budget: none  →  normally normalised the same way (nil), EXCEPT
//     on an OpenAI-family model that has a real API-level "none" effort
//     (gpt-5.6+, see [modelinfo.OpenAISupportsNoneEffort]): there the explicit
//     value is preserved so it actually reaches the API instead of silently
//     falling back to the model's default ("medium").
//   - thinking_budget explicitly set to a real value  →  kept as-is; interleaved_thinking
//     is auto-enabled for Anthropic/Bedrock-Claude.
//   - thinking_budget NOT set:
//   - Thinking-only models (OpenAI o-series) get "medium".
//   - All other models get no thinking.
//
// NOTE: max_tokens is NOT set here; see teamloader and runtime/model_switcher.
func applyModelDefaults(cfg *latest.ModelConfig) {
	providerType := resolveProviderType(cfg)

	// Explicitly disabled → normalise to nil so providers never see it,
	// unless the model has a real "none" effort worth preserving.
	if cfg.ThinkingBudget.IsDisabled() {
		if preservesNoneEffort(cfg, providerType) {
			slog.Debug("Preserving explicit none reasoning effort",
				"provider", cfg.Provider, "model", cfg.Model)
			return
		}
		cfg.ThinkingBudget = nil
		slog.Debug("Thinking explicitly disabled",
			"provider", cfg.Provider, "model", cfg.Model)
		return
	}

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

// preservesNoneEffort reports whether a disabled ThinkingBudget should be kept
// as an explicit "none" rather than normalised to nil. It applies only to an
// explicit string `effort: none` (as opposed to `tokens: 0`, which has no
// dedicated API value to preserve) on an OpenAI-protocol provider that is
// also a genuine OpenAI vendor (see [isOpenAIVendor]) whose model accepts a
// real "none" reasoning effort. An OpenAI-compatible alias for a different
// vendor (xai, mistral, ...) never preserves it, even if its model name
// happens to match gpt-5.6's naming pattern.
func preservesNoneEffort(cfg *latest.ModelConfig, providerType string) bool {
	if cfg.ThinkingBudget == nil || !strings.EqualFold(cfg.ThinkingBudget.Effort, "none") {
		return false
	}
	switch providerType {
	case "openai", "openai_chatcompletions", "openai_responses":
		return isOpenAIVendor(cfg) && modelinfo.OpenAISupportsNoneEffort(cfg.Model)
	default:
		return false
	}
}

// isOpenAIVendor reports whether cfg's model genuinely runs against OpenAI's
// own model catalog, as opposed to a different vendor (xai/grok, mistral,
// deepseek, ...) that merely speaks the OpenAI-compatible wire protocol.
//
// resolveProviderType only identifies the transport dialect (Chat
// Completions vs Responses vs some other OpenAI-compatible variant); many
// unrelated providers share it through their built-in alias APIType. Vendor
// identity needs a narrower, separate check so OpenAI-only behavior (like
// gpt-5.6's real "none" reasoning effort) doesn't leak onto e.g. xai/mistral
// just because they happen to share the wire format.
//
// Delegates the provider-name/model-qualifier core to
// [modelinfo.IsOpenAIVendor] and layers one more exclusion on top: a
// provider name that is NOT itself a known built-in alias (i.e. a custom
// provider from the providers: section) whose resolved API type is an
// OpenAI dialect. A protocol-only alias like xai/mistral never reaches this
// branch — it IS a known alias — so its APIType fallback can never be
// mistaken for a deliberate "this is an OpenAI endpoint" declaration.
//
// This result is threaded to the pkg/model/provider/openai client (which
// cannot import this package — see modelinfo.IsOpenAIVendor's doc for the
// import-cycle rationale) as trusted internal state via
// options.WithOpenAIVendor, applied once by createDirectProvider after this
// function runs on the fully-resolved (post-applyProviderDefaults) config. It
// is never written to ProviderOpts: that map is public, user-controllable
// config, so a user-supplied `provider_opts.openai_vendor` key must never be
// able to spoof or suppress this decision.
func isOpenAIVendor(cfg *latest.ModelConfig) bool {
	if modelinfo.IsOpenAIVendor(cfg.Provider, cfg.Model) {
		return true
	}
	return isUnrecognizedOpenAIProtocolProvider(cfg)
}

// isUnrecognizedOpenAIProtocolProvider reports whether cfg.Provider is NOT a
// known built-in alias (i.e. it's a custom provider from the providers:
// section, typically one that omits the underlying `provider:` field) whose
// resolved api_type speaks one of the OpenAI wire dialects. This is the one
// exclusion [modelinfo.IsOpenAIVendor] cannot express on its own, since the
// built-in alias registry lives in this package (see [isOpenAIVendor]'s doc).
func isUnrecognizedOpenAIProtocolProvider(cfg *latest.ModelConfig) bool {
	if _, isBuiltinAlias := LookupAlias(cfg.Provider); isBuiltinAlias {
		return false
	}
	switch resolveProviderType(cfg) {
	case "openai", "openai_chatcompletions", "openai_responses":
		return true
	default:
		return false
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
