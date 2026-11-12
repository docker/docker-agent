package modelcatalog

import (
	"context"
	"log/slog"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/catalog"
	"github.com/docker/docker-agent/pkg/model/provider/dmr/dmrmodels"
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

	// Local model metadata reported by Docker Model Runner.
	Architecture string `json:"architecture,omitempty"`
	Parameters   string `json:"parameters,omitempty"`
	Quantization string `json:"quantization,omitempty"`
	Size         string `json:"size,omitempty"`

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

// Config holds the configuration needed for model switching.
// This is populated by the app layer when creating the runtime.
type Config struct {
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
	ModelsStore Store
}

// Store provides model metadata and the complete catalog.
type Store interface {
	GetModel(ctx context.Context, id modelsdev.ID) (*modelsdev.Model, error)
	GetDatabase(ctx context.Context) (*modelsdev.Database, error)
}

// Catalog owns model discovery and picker metadata, independently of execution.
// Configuration is immutable after construction.
type Catalog struct {
	Config        *Config
	Store         Store
	Now           func() time.Time
	ListDMR       func(context.Context) ([]dmrmodels.Model, error)
	gatewayModels discoveryCache[[]string]
	dmrModels     discoveryCache[[]dmrmodels.Model]
}

// AvailableModels returns configured and discovered choices for agentName.
func (r *Catalog) AvailableModels(ctx context.Context, agentName string) []ModelChoice {
	start := time.Now()
	if r.Config == nil {
		slog.DebugContext(ctx, "Runtime available models skipped; model switching not configured", "duration", time.Since(start))
		return nil
	}

	// Get the current agent's default model reference
	currentAgentDefault := ""
	if r.Config.AgentDefaultModels != nil {
		currentAgentDefault = r.Config.AgentDefaultModels[agentName]
	}

	var choices []ModelChoice

	configuredStart := time.Now()
	// Add all configured models, marking the current agent's default
	for name, cfg := range r.Config.Models {
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
		if n := latest.ContextSizeFromProviderOpts(cfg.ProviderOpts); n > 0 {
			if n > math.MaxInt {
				choice.ContextLimit = math.MaxInt
			} else {
				choice.ContextLimit = int(n)
			}
		}
		choices = append(choices, choice)
	}
	configuredDuration := time.Since(configuredStart)

	// Prefer live gateway discovery when a models gateway is configured:
	// the picker then only shows the models actually served by the
	// gateway, merged with the explicitly configured ones above. When the
	// gateway doesn't expose /v1/models, fall back to the models.dev
	// catalog filtered by available credentials.
	if r.Config.ModelsGateway != "" {
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
	r.populateDMRChoices(ctx, choices)
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
		"configured_models", len(r.Config.Models),
		"dmr_models", len(dmrChoices),
		"catalog_models", len(catalogChoices),
		"models", len(choices),
	)
	return choices
}

// buildCatalogChoices builds ModelChoice entries from the models.dev catalog,
// filtered by supported providers and available credentials.
func (r *Catalog) buildCatalogChoices(ctx context.Context) []ModelChoice {
	start := time.Now()
	dbStart := time.Now()
	db, err := r.Store.GetDatabase(ctx)
	dbDuration := time.Since(dbStart)
	if err != nil {
		slog.DebugContext(ctx, "Failed to get models.dev database for catalog", "duration", time.Since(start), "database_duration", dbDuration, "error", err)
		return nil
	}

	// Build set of existing model refs to avoid duplicates
	existingRefs := make(map[string]bool)
	for name, cfg := range r.Config.Models {
		existingRefs[name] = true
		if cfg.Provider != "" && cfg.Model != "" {
			providerID := cfg.Provider
			if _, custom := r.Config.Providers[providerID]; !custom {
				providerID = modelsdev.CanonicalProviderID(providerID)
			}
			existingRefs[providerID+"/"+cfg.Model] = true
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
// Uses catalog metadata to include providers that need no endpoint configuration.
func mapModelsDevProvider(providerID string) (string, bool) {
	if catalog.IsCatalogProvider(providerID) {
		return providerID, true
	}
	return "", false
}

// populateCatalogMetadata fetches models.dev metadata for the given
// provider/model pair and copies it onto choice. It silently does
// nothing when the lookup fails or when the runtime has no models store.
func (r *Catalog) populateCatalogMetadata(ctx context.Context, choice *ModelChoice, providerID, modelID string) {
	if r.Store == nil {
		return
	}
	m, err := r.Store.GetModel(ctx, modelsdev.NewID(providerID, modelID))
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
func (r *Catalog) getAvailableProviders(ctx context.Context) map[string]bool {
	env := r.Config.EnvProvider

	available := config.DiscoveryAvailableProviders(ctx, env)

	// Mark anthropic available when any model or referenced provider in the
	// workspace has a non-API-key auth scheme configured (e.g. Workload
	// Identity Federation). We deliberately do not eagerly probe the token
	// source here: doing so would slow down startup for the common case
	// (file/env are fine, gcloud/az may take seconds, IMDS endpoints may
	// hang on non-cloud hosts). A misconfigured source surfaces as a clear
	// error on the first request via federation.RequestOptions.
	for _, m := range r.Config.Models {
		if modelHasAnthropicAuth(m, r.Config.Providers) {
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
