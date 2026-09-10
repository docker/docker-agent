package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelinfo"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

// ModelChoice represents a model available for selection in the model picker.
//
// JSON tags are part of the public wire format used by
// GET /api/sessions/:id/models; renaming a tag is a breaking change.
type ModelChoice struct {
	// Name is the display name (config key)
	Name string `json:"name"`
	// Ref is the model reference used internally (e.g., "my_model" or "openai/gpt-4o")
	Ref string `json:"ref"`
	// Provider is the provider name (e.g., "openai", "anthropic")
	Provider string `json:"provider,omitempty"`
	// Model is the specific model name (e.g., "gpt-4o", "claude-sonnet-4-0")
	Model string `json:"model,omitempty"`
	// IsDefault indicates this is the agent's configured default model
	IsDefault bool `json:"is_default,omitempty"`
	// IsCurrent indicates this is the currently active model for the agent
	IsCurrent bool `json:"is_current,omitempty"`
	// IsCustom indicates this is a custom model from the session history (not from config)
	IsCustom bool `json:"is_custom,omitempty"`
	// IsCatalog indicates this is a model from the models.dev catalog
	IsCatalog bool `json:"is_catalog,omitempty"`

	// The fields below are populated (best-effort) from the models.dev
	// catalog. They are optional and may all be zero/empty when no
	// catalog entry is found for the model.

	// Family is the model family (e.g., "claude", "gpt").
	Family string `json:"family,omitempty"`
	// InputCost is the price (in USD) per 1M input tokens.
	InputCost float64 `json:"input_cost,omitempty"`
	// OutputCost is the price (in USD) per 1M output tokens.
	OutputCost float64 `json:"output_cost,omitempty"`
	// CacheReadCost is the price (in USD) per 1M cached input tokens.
	CacheReadCost float64 `json:"cache_read_cost,omitempty"`
	// CacheWriteCost is the price (in USD) per 1M cache-write tokens.
	CacheWriteCost float64 `json:"cache_write_cost,omitempty"`
	// ContextLimit is the maximum context window size in tokens.
	ContextLimit int `json:"context_limit,omitempty"`
	// OutputLimit is the maximum number of tokens the model can produce
	// in a single response.
	OutputLimit int64 `json:"output_limit,omitempty"`
	// InputModalities lists the input modalities supported by the model
	// (e.g., "text", "image", "audio").
	InputModalities []string `json:"input_modalities,omitempty"`
	// OutputModalities lists the output modalities the model can produce.
	OutputModalities []string `json:"output_modalities,omitempty"`
}

// SessionModelsResponse is the response returned by
// GET /api/sessions/:id/models. CurrentModelRef is the active override for
// the named agent (empty when the agent is using its configured default).
type SessionModelsResponse struct {
	Agent           string        `json:"agent"`
	CurrentModelRef string        `json:"current_model_ref,omitempty"`
	Models          []ModelChoice `json:"models"`
}

// DecorateModelChoices marks the active selection with IsCurrent and
// appends any custom (provider/model) refs from the session history that
// the runtime does not already expose. It is used by every consumer that
// wants to render a model picker (the TUI App, the HTTP /sessions/:id/models
// endpoint, …) so they all agree on which entry is current and what the
// final list looks like.
//
// currentRef is the model override active for the agent ("" when none),
// and customRefs is the session's CustomModelsUsed history.
//
// The input slice is never mutated: callers can safely pass a slice that
// is shared with or backed by an internal cache.
func DecorateModelChoices(models []ModelChoice, currentRef string, customRefs []string) []ModelChoice {
	// Defensive copy: AvailableModels implementations may return a slice
	// backed by an internal cache. Mutating its IsCurrent flag in place
	// would leak picker state across sessions/agents.
	result := make([]ModelChoice, len(models), len(models)+len(customRefs)+1)
	copy(result, models)

	existingRefs := make(map[string]bool, len(result))
	for _, m := range result {
		existingRefs[m.Ref] = true
	}

	currentFound := currentRef == ""
	for i := range result {
		if currentRef != "" {
			if result[i].Ref == currentRef {
				result[i].IsCurrent = true
				currentFound = true
			}
		} else {
			result[i].IsCurrent = result[i].IsDefault
		}
	}

	for _, ref := range customRefs {
		if existingRefs[ref] {
			continue
		}
		existingRefs[ref] = true

		prov, name, _ := strings.Cut(ref, "/")
		isCurrent := ref == currentRef
		if isCurrent {
			currentFound = true
		}
		result = append(result, ModelChoice{
			Name:      ref,
			Ref:       ref,
			Provider:  prov,
			Model:     name,
			IsCurrent: isCurrent,
			IsCustom:  true,
		})
	}

	// If the override points at an inline provider/model not in the
	// runtime's list nor in the session's history, fabricate a synthetic
	// choice so the picker can still highlight the active selection.
	if !currentFound && strings.Contains(currentRef, "/") {
		prov, name, _ := strings.Cut(currentRef, "/")
		result = append(result, ModelChoice{
			Name:      currentRef,
			Ref:       currentRef,
			Provider:  prov,
			Model:     name,
			IsCurrent: true,
			IsCustom:  true,
		})
	}

	return result
}

// ModelSwitcherConfig holds the configuration needed for model switching.
// This is populated by the app layer when creating the runtime.
type ModelSwitcherConfig struct {
	// Models is the map of model names to configurations from the loaded config
	Models map[string]latest.ModelConfig
	// Providers is the map of custom provider configurations
	Providers map[string]latest.ProviderConfig
	// ModelsGateway is the gateway URL if configured
	ModelsGateway string
	// EncryptedConfig is the opaque encrypted agent YAML forwarded to a trusted
	// Docker models gateway on every request (X-Cagent-Encrypted-Config header).
	// Empty disables forwarding.
	EncryptedConfig string
	// EnvProvider provides access to environment variables
	EnvProvider environment.Provider
	// ProviderRegistry instantiates providers for runtime model switching.
	ProviderRegistry *provider.Registry
	// AgentDefaultModels maps agent names to their configured default model references
	AgentDefaultModels map[string]string
	// ModelsStore is the models.dev catalog store used for the picker's
	// pricing/context/modality metadata and the catalog listing. When set, the
	// runtime adopts it instead of building its own (cold) lazy store, so a
	// store the team loader already warmed is reused. Optional: a nil store
	// falls back to the lazy default. An explicit WithModelStore takes
	// precedence over this field.
	ModelsStore ModelStore
}

// SetAgentModel implements [Runtime.SetAgentModel] for LocalRuntime.
func (r *LocalRuntime) SetAgentModel(ctx context.Context, agentName, modelRef string) error {
	_, err := r.setAgentModelInternal(ctx, agentName, modelRef)
	return err
}

// SupportsModelSwitching reports whether the runtime was built with a
// [ModelSwitcherConfig], i.e. whether [SetAgentModel] / [AvailableModels]
// will return real data instead of the no-config empty path.
func (r *LocalRuntime) SupportsModelSwitching() bool {
	return r.modelSwitcherCfg != nil
}

// RefreshModelsCatalog forces a refetch of the models.dev catalog backing
// the model picker, bypassing the store's refresh interval. It returns
// [ErrUnsupported] when the runtime's model store cannot refresh (e.g. an
// in-memory test store).
func (r *LocalRuntime) RefreshModelsCatalog(ctx context.Context) error {
	refresher, ok := r.modelsStore.(interface {
		Refresh(ctx context.Context) error
	})
	if !ok {
		return ErrUnsupported
	}
	return refresher.Refresh(ctx)
}

// CycleAgentThinkingLevel implements [Runtime.CycleAgentThinkingLevel] for
// LocalRuntime. It reads the agent's current effective model, advances the
// thinking-effort level by one step through the levels that specific model
// supports (see [modelinfo.SupportedThinkingLevels]), re-creates the
// provider(s) with the new level, and installs them as a runtime override.
func (r *LocalRuntime) CycleAgentThinkingLevel(ctx context.Context, agentName string) (effort.Level, error) {
	return r.applyAgentThinkingLevel(ctx, agentName, func(supported []effort.Level, current effort.Level) (effort.Level, error) {
		// Clamp first so a configured level the model does not support re-enters
		// the cycle at the nearest supported tier instead of resetting it.
		return effort.NextSupportedLevel(supported, effort.Clamp(supported, current)), nil
	})
}

// SetAgentThinkingLevel implements [Runtime.SetAgentThinkingLevel] for
// LocalRuntime. The requested level must be one the agent's current model
// supports; otherwise an error listing the supported levels is returned.
func (r *LocalRuntime) SetAgentThinkingLevel(ctx context.Context, agentName string, level effort.Level) (effort.Level, error) {
	return r.applyAgentThinkingLevel(ctx, agentName, func(supported []effort.Level, _ effort.Level) (effort.Level, error) {
		if !slices.Contains(supported, level) {
			return "", fmt.Errorf("thinking level %q is not supported by this model (supported: %s)", level, levelNames(supported))
		}
		return level, nil
	})
}

// levelNames renders levels as a comma-separated list for error messages.
func levelNames(levels []effort.Level) string {
	names := make([]string, len(levels))
	for i, l := range levels {
		names[i] = string(l)
	}
	return strings.Join(names, ", ")
}

// resolveThinkingLevelsForModels returns the thinking-effort levels
// supported by models[0] (the agent's primary effective model) plus its
// currently active level. It is the pure resolution step shared by
// resolveAgentThinkingLevels (the read-only getter path, which snapshots
// EffectiveModels itself) and applyAgentThinkingLevel (the setter path,
// which must reuse the exact same snapshot it later re-creates providers
// from — see resolveAgentThinkingLevels for why that sharing matters).
func (r *LocalRuntime) resolveThinkingLevelsForModels(ctx context.Context, models []provider.Provider) ([]effort.Level, effort.Level, error) {
	if len(models) == 0 {
		return nil, "", errors.New("agent has no model configured")
	}

	baseCfg := models[0].BaseConfig().ModelConfig
	if !r.modelSupportsThinking(ctx, &baseCfg) {
		return nil, "", fmt.Errorf("model %q does not support thinking levels: %w", baseCfg.DisplayOrModel(), ErrUnsupported)
	}

	return modelinfo.SupportedThinkingLevels(baseCfg.Provider, baseCfg.Model), currentThinkingLevel(&baseCfg), nil
}

// resolveAgentThinkingLevels resolves agentName's current effective model
// and returns the thinking-effort levels it supports plus its currently
// active level. This is the single source of truth shared by
// applyAgentThinkingLevel (which validates a pick against it) and
// CurrentAgentThinkingLevels (which exposes it read-only for /effort
// argument completion), so a level offered by one can never be rejected by
// the other (#3731).
//
// It is read-only, so a single EffectiveModels snapshot is correct here.
// applyAgentThinkingLevel must NOT call this: it needs the same snapshot
// for both resolution and provider recreation (a second, later
// EffectiveModels() call could race a concurrent /model change or scoped
// override and validate against one model while applying to another).
func (r *LocalRuntime) resolveAgentThinkingLevels(ctx context.Context, agentName string) ([]effort.Level, effort.Level, error) {
	if r.modelSwitcherCfg == nil {
		return nil, "", ErrUnsupported
	}

	a, err := r.team.Agent(agentName)
	if err != nil {
		return nil, "", fmt.Errorf("agent not found: %w", err)
	}

	return r.resolveThinkingLevelsForModels(ctx, a.EffectiveModels())
}

// CurrentAgentThinkingLevels returns the thinking-effort levels the current
// agent's active model supports, or nil when the runtime has no model
// switcher configured, the agent/model can't be resolved, or the model does
// not support thinking at all. Backs the /effort argument completer
// (#3731): candidates must come from this resolution, not the static effort
// vocabulary, because SetAgentThinkingLevel hard-rejects any level outside
// it.
func (r *LocalRuntime) CurrentAgentThinkingLevels(ctx context.Context) []effort.Level {
	supported, _, err := r.resolveAgentThinkingLevels(ctx, r.currentAgentName())
	if err != nil {
		return nil
	}
	return supported
}

// applyAgentThinkingLevel resolves the agent's current model, asks pick to
// choose the target level among the model's supported ones, re-creates the
// effective provider(s) with that level, and installs them as a runtime
// override.
//
// It snapshots EffectiveModels exactly once and reuses that same snapshot
// for both capability resolution and provider recreation. Taking a second,
// later snapshot would let a concurrent /model change or scoped override
// land in between: the level would be validated against one model but
// applied to another, and — if the second snapshot came back empty — the
// override could be silently cleared while still reporting success.
func (r *LocalRuntime) applyAgentThinkingLevel(ctx context.Context, agentName string, pick func(supported []effort.Level, current effort.Level) (effort.Level, error)) (effort.Level, error) {
	if r.modelSwitcherCfg == nil {
		return "", ErrUnsupported
	}

	a, err := r.team.Agent(agentName)
	if err != nil {
		return "", fmt.Errorf("agent not found: %w", err)
	}

	models := a.EffectiveModels()
	supported, current, err := r.resolveThinkingLevelsForModels(ctx, models)
	if err != nil {
		return "", err
	}

	next, err := pick(supported, current)
	if err != nil {
		return "", err
	}

	// Re-create each effective provider (alloy models can have several) with
	// the new thinking level so the override preserves the existing pool.
	newProviders := make([]provider.Provider, 0, len(models))
	for _, m := range models {
		mc := m.BaseConfig().ModelConfig
		cfg := mc.Clone()
		cfg.ThinkingBudget = &latest.ThinkingBudget{Effort: string(next)}
		prov, err := r.createProviderFromConfig(ctx, cfg)
		if err != nil {
			return "", fmt.Errorf("failed to apply thinking level: %w", err)
		}
		newProviders = append(newProviders, prov)
	}

	a.SetModelOverride(newProviders...)
	slog.InfoContext(ctx, "Set agent thinking level", "agent", agentName, "level", next)
	return next, nil
}

// modelSupportsThinking reports whether cfg names a model that accepts a
// user-selectable thinking-effort level. It first trusts an explicit,
// enabled thinking_budget (covers reasoning models the heuristics below
// don't recognise), then falls back to per-provider model heuristics.
func (r *LocalRuntime) modelSupportsThinking(ctx context.Context, cfg *latest.ModelConfig) bool {
	if cfg.ThinkingBudget != nil && !cfg.ThinkingBudget.IsDisabled() {
		return true
	}
	if modelinfo.UsesReasoningEffort(cfg.Model) || modelinfo.UsesThinkingLevel(cfg.Model) {
		return true
	}
	model := strings.ToLower(cfg.Model)
	if strings.HasPrefix(model, "gemini-2.5") {
		return true
	}
	// Claude family (Anthropic / Bedrock / Vertex) supports adaptive thinking.
	if modelinfo.IsBedrockClaudeID(cfg.Model) || strings.HasPrefix(model, "claude-") {
		return true
	}
	if r.modelsStore != nil {
		if m, err := r.modelsStore.GetModel(ctx, modelsdev.NewID(cfg.Provider, cfg.Model)); err == nil && m != nil {
			return modelinfo.IsClaudeFamily(m.Family)
		}
	}
	return false
}

// currentThinkingLevel maps a model config's thinking_budget onto an
// [effort.Level]. A nil, disabled, token-based, or adaptive budget is
// reported as [effort.None] so the first cycle step lands on a concrete
// effort level.
func currentThinkingLevel(cfg *latest.ModelConfig) effort.Level {
	if l, ok := cfg.ThinkingBudget.EffortLevel(); ok {
		return l
	}
	return effort.None
}

// setAgentModelInternal applies modelRef as the agent's model override and
// returns a snapshot of the value that was just stored. The snapshot is
// captured atomically with the store (it is the pointer returned by
// SetModelOverride itself), so there is no window where another caller
// could intervene and the snapshot would refer to a different value.
//
// SetAgentModel is a thin wrapper that discards the snapshot; callers that
// want to do a CAS-based restore (see WithAgentModel) use this method
// directly to keep the snapshot.
func (r *LocalRuntime) setAgentModelInternal(ctx context.Context, agentName, modelRef string) (agent.ModelOverrideSnapshot, error) {
	if r.modelSwitcherCfg == nil {
		return agent.ModelOverrideSnapshot{}, errors.New("model switching not configured for this runtime")
	}

	a, err := r.team.Agent(agentName)
	if err != nil {
		return agent.ModelOverrideSnapshot{}, fmt.Errorf("agent not found: %w", err)
	}

	// Empty modelRef means clear the override (use agent's default)
	if modelRef == "" {
		snap := a.SetModelOverride()
		slog.InfoContext(ctx, "Cleared agent model override (using default)", "agent", agentName)
		return snap, nil
	}

	// Check if modelRef is a named model from config
	if modelConfig, exists := r.modelSwitcherCfg.Models[modelRef]; exists {
		modelConfig.Name = modelRef
		// Check if this is an alloy model (no provider, comma-separated models)
		if isAlloyModelConfig(modelConfig) {
			providers, err := r.resolveModelRefs(ctx, modelConfig.Model)
			if err != nil {
				return agent.ModelOverrideSnapshot{}, fmt.Errorf("failed to create alloy model from config: %w", err)
			}
			snap := a.SetModelOverride(providers...)
			slog.InfoContext(ctx, "Set agent model override (alloy)", "agent", agentName, "config_name", modelRef, "model_count", len(providers))
			return snap, nil
		}

		prov, err := r.createProviderFromConfig(ctx, &modelConfig)
		if err != nil {
			return agent.ModelOverrideSnapshot{}, fmt.Errorf("failed to create model from config: %w", err)
		}
		snap := a.SetModelOverride(prov)
		slog.InfoContext(ctx, "Set agent model override", "agent", agentName, "model", prov.ID().String(), "config_name", modelRef)
		return snap, nil
	}

	// Check if this is an inline alloy spec (comma-separated provider/model specs)
	// e.g., "openai/gpt-4o,anthropic/claude-sonnet-4-0"
	if isInlineAlloySpec(modelRef) {
		providers, err := r.resolveModelRefs(ctx, modelRef)
		if err != nil {
			return agent.ModelOverrideSnapshot{}, fmt.Errorf("failed to create inline alloy model: %w", err)
		}
		snap := a.SetModelOverride(providers...)
		slog.InfoContext(ctx, "Set agent model override (inline alloy)", "agent", agentName, "model_count", len(providers))
		return snap, nil
	}

	// Try single inline spec (provider/model)
	prov, err := r.resolveModelRef(ctx, modelRef)
	if err != nil {
		return agent.ModelOverrideSnapshot{}, fmt.Errorf("failed to resolve model %q: %w", modelRef, err)
	}
	snap := a.SetModelOverride(prov)
	slog.InfoContext(ctx, "Set agent model override (inline)", "agent", agentName, "model", prov.ID().String())
	return snap, nil
}

// WithAgentModel applies modelRef as a model override on the named agent
// and returns a function that restores the previous override safely.
//
// The returned restore func is always non-nil. On success it uses
// pointer-identity compare-and-swap on the agent's override, so a
// concurrent change made between the apply and the restore (e.g. by the
// TUI model picker) is preserved instead of being clobbered. The post-
// apply snapshot is captured atomically with the store inside
// SetModelOverride, so there is no window where a concurrent change
// could be misattributed to this scope. On error the agent is left
// untouched and restore is a no-op, so callers can always defer it
// without nil-checking.
func (r *LocalRuntime) WithAgentModel(ctx context.Context, agentName, modelRef string) (restore func(), err error) {
	noop := func() {}
	a, err := r.team.Agent(agentName)
	if err != nil {
		return noop, fmt.Errorf("agent not found: %w", err)
	}
	prev := a.SnapshotModelOverride()
	ours, err := r.setAgentModelInternal(ctx, agentName, modelRef)
	if err != nil {
		return noop, err
	}
	return func() { a.RestoreModelOverride(prev, ours) }, nil
}

// resolveModelRef resolves a model reference to a single provider.
// The reference can be a named model from the config or an inline
// "provider/model" spec (e.g. "openai/gpt-4o-mini").
func (r *LocalRuntime) resolveModelRef(ctx context.Context, modelRef string) (provider.Provider, error) {
	if r.modelSwitcherCfg == nil {
		return nil, errors.New("model switching not configured for this runtime")
	}

	// Try named model from config first.
	if modelCfg, exists := r.modelSwitcherCfg.Models[modelRef]; exists {
		if isAlloyModelConfig(modelCfg) {
			return nil, fmt.Errorf("model reference %q is an alloy (multi-model) config and cannot be used as a single model override", modelRef)
		}
		modelCfg.Name = modelRef
		return r.createProviderFromConfig(ctx, &modelCfg)
	}

	// Try inline "provider/model" format.
	inlineCfg, err := latest.ParseModelRef(modelRef)
	if err != nil {
		return nil, fmt.Errorf("invalid model reference %q: expected a model name from config or 'provider/model' format", modelRef)
	}

	return r.createProviderFromConfig(ctx, &inlineCfg)
}

// isAlloyModelConfig checks if a model config is an alloy model (multiple models).
func isAlloyModelConfig(cfg latest.ModelConfig) bool {
	return cfg.Provider == "" && strings.Contains(cfg.Model, ",")
}

// isInlineAlloySpec checks if a model reference is an inline alloy specification.
// An inline alloy is comma-separated provider/model specs like "openai/gpt-4o,anthropic/claude-sonnet-4-0".
func isInlineAlloySpec(modelRef string) bool {
	if !strings.Contains(modelRef, ",") {
		return false
	}
	// Check that each part looks like a provider/model spec
	// and count valid parts (need at least 2 for an alloy)
	validParts := 0
	for part := range strings.SplitSeq(modelRef, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.Contains(part, "/") {
			return false
		}
		validParts++
	}
	return validParts >= 2
}

// resolveModelRefs resolves a comma-separated list of model references into
// providers. Each reference is first looked up in the config by name; if not
// found it is parsed as an inline "provider/model" spec.
func (r *LocalRuntime) resolveModelRefs(ctx context.Context, commaSeparatedRefs string) ([]provider.Provider, error) {
	var providers []provider.Provider

	for ref := range strings.SplitSeq(commaSeparatedRefs, ",") {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}

		// Check if this ref exists as a named model in config
		if modelCfg, exists := r.modelSwitcherCfg.Models[ref]; exists {
			modelCfg.Name = ref
			prov, err := r.createProviderFromConfig(ctx, &modelCfg)
			if err != nil {
				return nil, fmt.Errorf("failed to create provider for %q: %w", ref, err)
			}
			providers = append(providers, prov)
			continue
		}

		// Parse as provider/model
		inlineCfg, parseErr := latest.ParseModelRef(ref)
		if parseErr != nil {
			return nil, fmt.Errorf("invalid model reference %q: expected 'provider/model' format or a named model from config", ref)
		}

		prov, err := r.createProviderFromConfig(ctx, &inlineCfg)
		if err != nil {
			return nil, fmt.Errorf("failed to create provider for %q: %w", ref, err)
		}
		providers = append(providers, prov)
	}

	if len(providers) == 0 {
		return nil, errors.New("no valid models found in model reference list")
	}

	return providers, nil
}

// AvailableModels implements [Runtime.AvailableModels] for LocalRuntime.
func (r *LocalRuntime) AvailableModels(ctx context.Context) []ModelChoice {
	start := time.Now()
	if r.modelSwitcherCfg == nil {
		slog.DebugContext(ctx, "Runtime available models skipped; model switching not configured", "duration", time.Since(start))
		return nil
	}

	// Get the current agent's default model reference
	currentAgentDefault := ""
	if r.modelSwitcherCfg.AgentDefaultModels != nil {
		currentAgentDefault = r.modelSwitcherCfg.AgentDefaultModels[r.currentAgentName()]
	}

	var choices []ModelChoice

	configuredStart := time.Now()
	// Add all configured models, marking the current agent's default
	for name, cfg := range r.modelSwitcherCfg.Models {
		choice := ModelChoice{
			Name:      name,
			Ref:       name,
			Provider:  cfg.Provider,
			Model:     cfg.DisplayOrModel(),
			IsDefault: name == currentAgentDefault,
		}
		// Best-effort lookup of pricing / context information from models.dev.
		if cfg.Provider != "" && cfg.Model != "" {
			r.populateCatalogMetadata(ctx, &choice, cfg.Provider, cfg.Model)
		}
		// A config-declared price table overrides the catalogue's.
		if cfg.Cost != nil {
			choice.InputCost = cfg.Cost.Input
			choice.OutputCost = cfg.Cost.Output
			choice.CacheReadCost = cfg.Cost.CacheRead
			choice.CacheWriteCost = cfg.Cost.CacheWrite
		}
		choices = append(choices, choice)
	}
	configuredDuration := time.Since(configuredStart)

	// Prefer live gateway discovery when a models gateway is configured:
	// the picker then only shows the models actually served by the
	// gateway, merged with the explicitly configured ones above. When the
	// gateway doesn't expose /v1/models, fall back to the models.dev
	// catalog filtered by available credentials.
	if r.modelSwitcherCfg.ModelsGateway != "" {
		gatewayStart := time.Now()
		gatewayChoices, ok := r.buildGatewayChoices(ctx)
		slog.DebugContext(ctx, "Runtime available models gateway discovery completed",
			"duration", time.Since(gatewayStart),
			"ok", ok,
			"models", len(gatewayChoices),
		)
		if ok {
			result := append(choices, gatewayChoices...)
			slog.DebugContext(ctx, "Runtime available models completed",
				"duration", time.Since(start),
				"configured_duration", configuredDuration,
				"configured_models", len(choices),
				"gateway_models", len(gatewayChoices),
				"catalog_models", 0,
				"dmr_models", 0,
				"models", len(result),
			)
			return result
		}
	}

	// Surface models pulled locally in Docker Model Runner. They are not part
	// of the models.dev catalog, so without this a working local Model Runner
	// would show nothing selectable in the picker.
	dmrStart := time.Now()
	dmrChoices := r.buildDMRChoices(ctx)
	dmrDuration := time.Since(dmrStart)
	choices = append(choices, dmrChoices...)

	// Append models.dev catalog entries filtered by available credentials
	catalogStart := time.Now()
	catalogChoices := r.buildCatalogChoices(ctx)
	catalogDuration := time.Since(catalogStart)
	choices = append(choices, catalogChoices...)

	slog.DebugContext(ctx, "Runtime available models completed",
		"duration", time.Since(start),
		"configured_duration", configuredDuration,
		"dmr_duration", dmrDuration,
		"catalog_duration", catalogDuration,
		"configured_models", len(r.modelSwitcherCfg.Models),
		"dmr_models", len(dmrChoices),
		"catalog_models", len(catalogChoices),
		"models", len(choices),
	)
	return choices
}

// buildCatalogChoices builds ModelChoice entries from the models.dev catalog,
// filtered by supported providers and available credentials.
func (r *LocalRuntime) buildCatalogChoices(ctx context.Context) []ModelChoice {
	start := time.Now()
	dbStart := time.Now()
	db, err := r.modelsStore.GetDatabase(ctx)
	dbDuration := time.Since(dbStart)
	if err != nil {
		slog.DebugContext(ctx, "Failed to get models.dev database for catalog", "duration", time.Since(start), "database_duration", dbDuration, "error", err)
		return nil
	}

	// Build set of existing model refs to avoid duplicates
	existingRefs := make(map[string]bool)
	for name, cfg := range r.modelSwitcherCfg.Models {
		existingRefs[name] = true
		if cfg.Provider != "" && cfg.Model != "" {
			existingRefs[cfg.Provider+"/"+cfg.Model] = true
		}
	}

	// Check which providers the user has credentials for
	providerStart := time.Now()
	availableProviders := r.getAvailableProviders(ctx)
	providerDuration := time.Since(providerStart)
	if len(availableProviders) == 0 {
		slog.DebugContext(ctx, "No provider credentials available, skipping catalog",
			"duration", time.Since(start),
			"database_duration", dbDuration,
			"provider_duration", providerDuration,
		)
		return nil
	}

	iterateStart := time.Now()
	var choices []ModelChoice
	var providerCount, modelCount, skippedProvider, skippedNonText, skippedEmbedding, skippedDuplicate int
	for providerID, prov := range db.Providers {
		providerCount++
		// Check if this provider is supported and user has credentials
		dockerAgentProvider, supported := mapModelsDevProvider(providerID)
		if !supported {
			skippedProvider++
			continue
		}
		if !availableProviders[dockerAgentProvider] {
			skippedProvider++
			continue
		}

		for modelID, model := range prov.Models {
			modelCount++
			// Skip models that don't output text (not suitable for chat)
			if !slices.Contains(model.Modalities.Output, "text") {
				skippedNonText++
				continue
			}
			// Skip embedding models (not suitable for chat)
			if isEmbeddingModel(model.Family, model.Name) {
				skippedEmbedding++
				continue
			}

			ref := dockerAgentProvider + "/" + modelID
			if existingRefs[ref] {
				skippedDuplicate++
				continue
			}
			existingRefs[ref] = true

			choice := ModelChoice{
				Name:      model.Name,
				Ref:       ref,
				Provider:  dockerAgentProvider,
				Model:     modelID,
				IsCatalog: true,
			}
			applyCatalogMetadata(&choice, &model)
			choices = append(choices, choice)
		}
	}

	slog.DebugContext(ctx, "Built catalog choices",
		"duration", time.Since(start),
		"database_duration", dbDuration,
		"provider_duration", providerDuration,
		"iterate_duration", time.Since(iterateStart),
		"providers", providerCount,
		"models_seen", modelCount,
		"models", len(choices),
		"available_providers", len(availableProviders),
		"skipped_provider", skippedProvider,
		"skipped_non_text", skippedNonText,
		"skipped_embedding", skippedEmbedding,
		"skipped_duplicate", skippedDuplicate,
	)
	return choices
}

// mapModelsDevProvider maps a models.dev provider ID to a docker agent provider name.
// Returns the docker agent provider name and whether it's supported.
// Uses provider.IsCatalogProvider to dynamically include all core providers
// and aliases with defined base URLs.
func mapModelsDevProvider(providerID string) (string, bool) {
	if provider.IsCatalogProvider(providerID) {
		return providerID, true
	}
	return "", false
}

// populateCatalogMetadata fetches models.dev metadata for the given
// provider/model pair and copies it onto choice. It silently does
// nothing when the lookup fails or when the runtime has no models store.
func (r *LocalRuntime) populateCatalogMetadata(ctx context.Context, choice *ModelChoice, providerID, modelID string) {
	if r.modelsStore == nil {
		return
	}
	m, err := r.modelsStore.GetModel(ctx, modelsdev.NewID(providerID, modelID))
	if err == nil {
		applyCatalogMetadata(choice, m)
	}
}

// applyCatalogMetadata copies pricing/limit/modality information from a
// models.dev Model entry onto a ModelChoice.
func applyCatalogMetadata(choice *ModelChoice, m *modelsdev.Model) {
	if m == nil {
		return
	}
	choice.Family = m.Family
	if m.Cost != nil {
		choice.InputCost = m.Cost.Input
		choice.OutputCost = m.Cost.Output
		choice.CacheReadCost = m.Cost.CacheRead
		choice.CacheWriteCost = m.Cost.CacheWrite
	}
	choice.ContextLimit = m.Limit.Context
	choice.OutputLimit = m.Limit.Output
	choice.InputModalities = slices.Clone(m.Modalities.Input)
	choice.OutputModalities = slices.Clone(m.Modalities.Output)
}

// isEmbeddingModel returns true if the model is an embedding model
// based on its family or name fields from models.dev.
func isEmbeddingModel(family, name string) bool {
	familyLower := strings.ToLower(family)
	nameLower := strings.ToLower(name)
	return strings.Contains(familyLower, "embed") || strings.Contains(nameLower, "embed")
}

// getAvailableProviders returns the providers that model discovery/listing
// may surface for this runtime. Availability is credential-based (shared
// with the CLI models-listing fallback via
// [config.DiscoveryAvailableProviders]): a configured models gateway no
// longer replaces it with a hardcoded provider list, so when gateway
// discovery fails — or the gateway is a generic one without a Docker token —
// the catalog fallback still reflects the providers the user can actually
// reach directly.
func (r *LocalRuntime) getAvailableProviders(ctx context.Context) map[string]bool {
	env := r.modelSwitcherCfg.EnvProvider

	available := config.DiscoveryAvailableProviders(ctx, env)

	// Mark anthropic available when any model or referenced provider in the
	// workspace has a non-API-key auth scheme configured (e.g. Workload
	// Identity Federation). We deliberately do not eagerly probe the token
	// source here: doing so would slow down startup for the common case
	// (file/env are fine, gcloud/az may take seconds, IMDS endpoints may
	// hang on non-cloud hosts). A misconfigured source surfaces as a clear
	// error on the first request via federation.RequestOptions.
	for _, m := range r.modelSwitcherCfg.Models {
		if modelHasAnthropicAuth(m, r.modelSwitcherCfg.Providers) {
			available["anthropic"] = true
			break
		}
	}

	// DMR and ollama don't require credentials (local models)
	available["dmr"] = true
	available["ollama"] = true

	return available
}

// modelHasAnthropicAuth reports whether the model (or its referenced
// ProviderConfig) declares a non-API-key auth scheme that targets the
// anthropic provider. Used by getAvailableProviders so that workspaces
// configured with Workload Identity Federation surface their Anthropic
// models without requiring ANTHROPIC_API_KEY.
func modelHasAnthropicAuth(m latest.ModelConfig, providers map[string]latest.ProviderConfig) bool {
	return latest.EffectiveProviderType(m, providers) == "anthropic" &&
		latest.EffectiveAuth(m, providers) != nil
}

// createProviderFromConfig creates a provider from a ModelConfig using the runtime's configuration.
func (r *LocalRuntime) createProviderFromConfig(ctx context.Context, cfg *latest.ModelConfig) (provider.Provider, error) {
	opts := []options.Opt{
		options.WithGateway(r.modelSwitcherCfg.ModelsGateway),
		options.WithEncryptedConfig(r.modelSwitcherCfg.EncryptedConfig),
		options.WithProviders(r.modelSwitcherCfg.Providers),
	}

	// Use max_tokens from config if specified, otherwise look up from models.dev
	if cfg.MaxTokens != nil {
		opts = append(opts, options.WithMaxTokens(*cfg.MaxTokens))
	} else if r.modelsStore != nil {
		m, err := r.modelsStore.GetModel(ctx, modelsdev.NewID(cfg.Provider, cfg.Model))
		if err == nil && m != nil {
			opts = append(opts, options.WithMaxTokens(m.Limit.Output))
		}
	}

	if store, ok := r.modelsStore.(*modelsdev.Store); ok && store != nil {
		opts = append(opts, options.WithModelsDevStore(store))
	}

	registry := r.modelSwitcherCfg.ProviderRegistry
	if registry == nil {
		registry = r.providerRegistry
	}
	if registry == nil {
		registry = provider.DefaultRegistry()
	}
	return registry.NewWithModels(ctx,
		cfg,
		r.modelSwitcherCfg.Models,
		r.modelSwitcherCfg.EnvProvider,
		opts...,
	)
}

// WithModelSwitcherConfig sets the model switcher configuration for the runtime.
func WithModelSwitcherConfig(cfg *ModelSwitcherConfig) Opt {
	return func(r *LocalRuntime) {
		if cfg != nil && cfg.ProviderRegistry == nil {
			cfgCopy := *cfg
			cfgCopy.ProviderRegistry = r.providerRegistry
			cfg = &cfgCopy
		}
		if cfg != nil && cfg.ProviderRegistry != nil {
			r.providerRegistry = cfg.ProviderRegistry
		}
		r.modelSwitcherCfg = cfg
	}
}
