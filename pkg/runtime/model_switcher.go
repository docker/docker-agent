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
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

// ModelChoice represents a model available for selection in the TUI picker.
type ModelChoice struct {
	// Name is the display name (config key)
	Name string
	// Ref is the model reference used internally (e.g., "my_model" or "openai/gpt-4o")
	Ref string
	// Provider is the provider name (e.g., "openai", "anthropic")
	Provider string
	// Model is the specific model name (e.g., "gpt-4o", "claude-sonnet-4-0")
	Model string
	// IsDefault indicates this is the agent's configured default model
	IsDefault bool
	// IsCurrent indicates this is the currently active model for the agent
	IsCurrent bool
	// IsCustom indicates this is a custom model from the session history (not from config)
	IsCustom bool
	// IsCatalog indicates this is a model from the models.dev catalog
	IsCatalog bool

	// The fields below are populated (best-effort) from the models.dev
	// catalog. They are optional and may all be zero/empty when no
	// catalog entry is found for the model.

	// Family is the model family (e.g., "claude", "gpt").
	Family string
	// InputCost is the price (in USD) per 1M input tokens.
	InputCost float64
	// OutputCost is the price (in USD) per 1M output tokens.
	OutputCost float64
	// CacheReadCost is the price (in USD) per 1M cached input tokens.
	CacheReadCost float64
	// CacheWriteCost is the price (in USD) per 1M cache-write tokens.
	CacheWriteCost float64
	// ContextLimit is the maximum context window size in tokens.
	ContextLimit int
	// OutputLimit is the maximum number of tokens the model can produce
	// in a single response.
	OutputLimit int64
	// InputModalities lists the input modalities supported by the model
	// (e.g., "text", "image", "audio").
	InputModalities []string
	// OutputModalities lists the output modalities the model can produce.
	OutputModalities []string
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
	// EnvProvider provides access to environment variables
	EnvProvider environment.Provider
	// AgentDefaultModels maps agent names to their configured default model references
	AgentDefaultModels map[string]string
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
	if r.modelSwitcherCfg == nil {
		return nil
	}

	// Get the current agent's default model reference
	currentAgentDefault := ""
	if r.modelSwitcherCfg.AgentDefaultModels != nil {
		currentAgentDefault = r.modelSwitcherCfg.AgentDefaultModels[r.CurrentAgentName()]
	}

	var choices []ModelChoice

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
		choices = append(choices, choice)
	}

	// Append models.dev catalog entries filtered by available credentials
	catalogChoices := r.buildCatalogChoices(ctx)
	choices = append(choices, catalogChoices...)

	return choices
}

// buildCatalogChoices builds ModelChoice entries from the models.dev catalog,
// filtered by supported providers and available credentials.
func (r *LocalRuntime) buildCatalogChoices(ctx context.Context) []ModelChoice {
	db, err := r.modelsStore.GetDatabase(ctx)
	if err != nil {
		slog.DebugContext(ctx, "Failed to get models.dev database for catalog", "error", err)
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
	availableProviders := r.getAvailableProviders(ctx)
	if len(availableProviders) == 0 {
		slog.DebugContext(ctx, "No provider credentials available, skipping catalog")
		return nil
	}

	var choices []ModelChoice
	for providerID, prov := range db.Providers {
		// Check if this provider is supported and user has credentials
		dockerAgentProvider, supported := mapModelsDevProvider(providerID)
		if !supported {
			continue
		}
		if !availableProviders[dockerAgentProvider] {
			continue
		}

		for modelID, model := range prov.Models {
			// Skip models that don't output text (not suitable for chat)
			if !slices.Contains(model.Modalities.Output, "text") {
				continue
			}
			// Skip embedding models (not suitable for chat)
			if isEmbeddingModel(model.Family, model.Name) {
				continue
			}

			ref := dockerAgentProvider + "/" + modelID
			if existingRefs[ref] {
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

	slog.DebugContext(ctx, "Built catalog choices", "count", len(choices), "available_providers", len(availableProviders))
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

// getAvailableProviders returns a map of provider names that the user has credentials for.
func (r *LocalRuntime) getAvailableProviders(ctx context.Context) map[string]bool {
	available := make(map[string]bool)
	env := r.modelSwitcherCfg.EnvProvider

	// If using a models gateway, check for Docker token
	if r.modelSwitcherCfg.ModelsGateway != "" {
		if token, _ := env.Get(ctx, environment.DockerDesktopTokenEnv); token != "" {
			// Gateway supports all providers
			available["openai"] = true
			available["anthropic"] = true
			available["google"] = true
			available["mistral"] = true
			available["xai"] = true
		}
		return available
	}

	// Check credentials for each alias provider
	for name, alias := range provider.EachAlias() {
		if alias.TokenEnvVar == "" {
			continue
		}
		if key, _ := env.Get(ctx, alias.TokenEnvVar); key != "" {
			available[name] = true
		}
	}

	// Check core providers with well-known env vars
	if key, _ := env.Get(ctx, "OPENAI_API_KEY"); key != "" {
		available["openai"] = true
	}
	if key, _ := env.Get(ctx, "ANTHROPIC_API_KEY"); key != "" {
		available["anthropic"] = true
	}
	if key, _ := env.Get(ctx, "GOOGLE_API_KEY"); key != "" {
		available["google"] = true
	}

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

	// Amazon Bedrock uses AWS credentials which can come from many sources.
	// We do a quick heuristic check for common indicators without blocking:
	// - AWS_ACCESS_KEY_ID: explicit access key
	// - AWS_PROFILE / AWS_DEFAULT_PROFILE: named profile (credentials in ~/.aws/)
	// - AWS_WEB_IDENTITY_TOKEN_FILE: EKS/IRSA web identity
	// - AWS_CONTAINER_CREDENTIALS_RELATIVE_URI: ECS task role
	// - AWS_ROLE_ARN: assumed role
	// Note: This won't catch all cases (e.g., EC2 instance profiles, SSO) but
	// those require network calls which would block the UI.
	awsCredentialIndicators := []string{
		"AWS_ACCESS_KEY_ID",
		"AWS_PROFILE",
		"AWS_DEFAULT_PROFILE",
		"AWS_WEB_IDENTITY_TOKEN_FILE",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI",
		"AWS_ROLE_ARN",
	}
	for _, indicator := range awsCredentialIndicators {
		if val, _ := env.Get(ctx, indicator); val != "" {
			available["amazon-bedrock"] = true
			break
		}
	}

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

	return provider.NewWithModels(ctx,
		cfg,
		r.modelSwitcherCfg.Models,
		r.modelSwitcherCfg.EnvProvider,
		opts...,
	)
}

// WithModelSwitcherConfig sets the model switcher configuration for the runtime.
func WithModelSwitcherConfig(cfg *ModelSwitcherConfig) Opt {
	return func(r *LocalRuntime) {
		r.modelSwitcherCfg = cfg
	}
}
