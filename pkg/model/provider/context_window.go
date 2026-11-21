package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/modelinfo"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

// ValidateContextWindow checks declarations against the effective API type,
// including inherited provider options. It does not mutate the configuration.
func ValidateContextWindow(cfg *latest.ModelConfig, providers map[string]latest.ProviderConfig) error {
	if cfg.ContextWindow != nil && *cfg.ContextWindow <= 0 {
		return fmt.Errorf("context_window must be greater than 0, got %d", *cfg.ContextWindow)
	}
	effective := applyProviderDefaults(cfg, providers)
	raw, present := effective.ProviderOpts["context_size"]
	if !present {
		return nil
	}
	owner := "provider_opts"
	if _, own := cfg.ProviderOpts["context_size"]; !own {
		owner = fmt.Sprintf("providers.%s.provider_opts", cfg.Provider)
	}
	providerType := resolveProviderType(effective)
	if providerType != "dmr" {
		if owner != "provider_opts" {
			return fmt.Errorf("%s.context_size is only used by the dmr provider, not %q; remove it from the provider and set context_window on models using this provider instead (replace inline provider/model references with named models entries)", owner, providerType)
		}
		return fmt.Errorf("%s.context_size is only used by the dmr provider, not %q; move it to context_window on this model", owner, providerType)
	}
	if cfg.ContextWindow != nil {
		if declared := latest.ContextSizeFromProviderOpts(map[string]any{"context_size": raw}); declared > 0 && declared != *cfg.ContextWindow {
			return fmt.Errorf("context_window %d conflicts with %s.context_size %d; set only one, or make them equal", *cfg.ContextWindow, owner, declared)
		}
	}
	return nil
}

// ValidateContextWindowCap compares declarations with raw catalogue limits,
// using provider defaults and the environment-expanded DisplayOrModel identity.
// Endpoint expansion belongs to construction, not catalogue lookup.
// Unknown models or catalogue entries without limits accept any positive window.
func ValidateContextWindowCap(ctx context.Context, cfg *latest.ModelConfig, providers map[string]latest.ProviderConfig, store *modelsdev.Store, env environment.Provider) error {
	if cfg.ContextWindow == nil || cfg.IsFirstAvailable() {
		return nil
	}
	if store == nil {
		return errors.New("cannot check context_window against the models.dev catalogue: the catalogue is unavailable")
	}
	effective := applyProviderDefaults(cfg, providers)
	model, err := environment.Expand(ctx, effective.DisplayOrModel(), env)
	if err != nil {
		return fmt.Errorf("cannot resolve model for context_window validation: %w", err)
	}
	catalogueProvider := effective.Provider
	if resolveProviderType(effective) == "google" {
		publisher, _ := effective.ProviderOpts["publisher"].(string)
		if publisher != "" && !strings.EqualFold(publisher, "google") {
			catalogueProvider = modelinfo.VertexModelsDevProvider(publisher)
		}
	}
	id := modelsdev.NewID(catalogueProvider, model)
	entry, err := store.GetModel(ctx, id)
	if err != nil || entry == nil || entry.Limit.Context <= 0 {
		return nil
	}
	if window := int64(entry.Limit.Context); *cfg.ContextWindow > window {
		return fmt.Errorf("context_window %d exceeds the context window of %s, which is %d tokens in the models.dev catalogue; set context_window to at most %d, or remove it to use the catalogue window", *cfg.ContextWindow, id, window, window)
	}
	return nil
}
