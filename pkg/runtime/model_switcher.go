package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelinfo"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime/modelcatalog"
)

// SetAgentModel implements [Runtime.SetAgentModel] for LocalRuntime.
func (r *LocalRuntime) SetAgentModel(ctx context.Context, agentName, modelRef string) error {
	_, err := r.setAgentModelInternal(ctx, agentName, modelRef)
	return err
}

// SupportsModelSwitching reports whether the runtime was built with a
// [modelcatalog.Config], i.e. whether [LocalRuntime.SetAgentModel] / [LocalRuntime.AvailableModels]
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
		// the cycle at the nearest supported tier instead of resetting it. An
		// unset level (a harness on its CLI default) starts the cycle instead.
		if current != "" {
			current = effort.Clamp(supported, current)
		}
		return effort.NextSupportedLevel(supported, current), nil
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
	if a.HasHarness() {
		return harnessThinkingLevels(a.Harness())
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
// override. A harness agent gets a harness effort override instead.
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
	if a.HasHarness() {
		return applyHarnessThinkingLevel(ctx, a, pick)
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

// harnessEffortLevels are the harness.effort values config validation accepts.
var harnessEffortLevels = []effort.Level{effort.Low, effort.Medium, effort.High, effort.XHigh, effort.Max}

// harnessThinkingLevels returns the levels a harness agent's effort can take
// and its current one, "" while the CLI picks it.
func harnessThinkingLevels(cfg *latest.HarnessConfig) ([]effort.Level, effort.Level, error) {
	if cfg.Type != "claude-code" && cfg.Type != "command-code" {
		return nil, "", fmt.Errorf("harness %q does not support thinking levels: %w", cfg.Type, ErrUnsupported)
	}
	return slices.Clone(harnessEffortLevels), effort.Level(cfg.Effort), nil
}

// harnessThinkingLabel is agentThinkingLabel for a harness agent.
func harnessThinkingLabel(cfg *latest.HarnessConfig) string {
	_, current, err := harnessThinkingLevels(cfg)
	switch {
	case err != nil:
		return ""
	case current == "":
		return "adaptive"
	default:
		return string(current)
	}
}

// applyHarnessThinkingLevel is applyAgentThinkingLevel for a harness agent:
// its next turns pass the picked level to the CLI.
func applyHarnessThinkingLevel(ctx context.Context, a *agent.Agent, pick func(supported []effort.Level, current effort.Level) (effort.Level, error)) (effort.Level, error) {
	supported, current, err := harnessThinkingLevels(a.Harness())
	if err != nil {
		return "", err
	}

	next, err := pick(supported, current)
	if err != nil {
		return "", err
	}

	a.SetHarnessEffort(string(next))
	slog.InfoContext(ctx, "Set agent thinking level", "agent", a.Name(), "level", next)
	return next, nil
}

// modelSupportsThinking reports whether cfg names a model that accepts a
// user-selectable thinking-effort level. It first trusts an explicit,
// enabled thinking_budget (covers reasoning models the heuristics below
// don't recognise), then falls back to per-provider model heuristics.
func (r *LocalRuntime) modelSupportsThinking(ctx context.Context, cfg *latest.ModelConfig) bool {
	if modelSupportsThinkingLocally(cfg) {
		return true
	}
	if r.modelsStore != nil {
		if m, err := r.modelsStore.GetModel(ctx, modelsdev.NewID(cfg.Provider, cfg.Model)); err == nil && m != nil {
			return modelinfo.IsClaudeFamily(m.Family)
		}
	}
	return false
}

func modelSupportsThinkingLocally(cfg *latest.ModelConfig) bool {
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
	return false
}

// AgentThinkingConfiguration reports a conservative, network-free single-model selector.
// A blank current level means an unset, adaptive, or token-based budget, not "none".
func (r *LocalRuntime) AgentThinkingConfiguration(agentName string) ([]effort.Level, effort.Level) {
	if r.modelSwitcherCfg == nil {
		return nil, ""
	}
	a, err := r.team.Agent(agentName)
	if err != nil || a.HasHarness() {
		return nil, ""
	}
	models := a.EffectiveModels()
	if len(models) != 1 {
		return nil, ""
	}
	cfg := models[0].BaseConfig().ModelConfig
	capability := cfg
	// Budget-only recognition disappears when a provider normalizes "none" to nil.
	capability.ThinkingBudget = nil
	if len(cfg.Routing) != 0 || !modelSupportsThinkingLocally(&capability) {
		return nil, ""
	}
	level, _ := cfg.ThinkingBudget.EffortLevel()
	return modelinfo.SupportedThinkingLevels(cfg.Provider, cfg.Model), level
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

// AvailableModels returns configured and discovered choices for the active agent.
func (r *LocalRuntime) AvailableModels(ctx context.Context) []modelcatalog.ModelChoice {
	if r.modelSwitcherCfg == nil {
		return nil
	}
	return r.catalog().AvailableModels(ctx, r.currentAgentName())
}

func (r *LocalRuntime) catalog() *modelcatalog.Catalog {
	r.catalogOnce.Do(func() {
		r.modelCatalog = &modelcatalog.Catalog{
			Config:  r.modelSwitcherCfg,
			Store:   r.modelsStore,
			Now:     r.now,
			ListDMR: r.dmrModelLister,
		}
	})
	return r.modelCatalog
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
		registry = provider.EmptyRegistry()
	}
	return registry.NewWithModels(ctx,
		cfg,
		r.modelSwitcherCfg.Models,
		r.modelSwitcherCfg.EnvProvider,
		opts...,
	)
}

// WithModelSwitcherConfig sets the model switcher configuration for the runtime.
func WithModelSwitcherConfig(cfg *modelcatalog.Config) Opt {
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
