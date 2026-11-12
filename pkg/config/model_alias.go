package config

import (
	"context"
	"log/slog"
	"strings"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

// ResolveModelAliases resolves model aliases to their pinned versions in the config.
// For example, "claude-sonnet-4-5" might resolve to "claude-sonnet-4-5-20250929".
// This modifies the config in place.
//
// NOTE: Alias resolution is skipped for models with custom base_url configurations,
// either set directly on the model or inherited from a custom provider definition.
// This is necessary because external providers (like Azure Foundry) may use the alias
// names directly as deployment names rather than the pinned version names.
func ResolveModelAliases(ctx context.Context, cfg *latest.Config, store *modelsdev.Store) {
	// Resolve model aliases in the models section
	for name, modelCfg := range cfg.Models {
		if modelCfg.IsFirstAvailable() || isInlineModelEntry(name, modelCfg) {
			continue
		}

		// Skip alias resolution for models with custom base_url (direct or via provider)
		// Custom endpoints like Azure Foundry use alias names as deployment names
		if latest.HasCustomBaseURL(modelCfg, cfg.Providers) {
			slog.DebugContext(ctx, "Skipping model alias resolution for model with custom base_url",
				"model_name", name, "provider", modelCfg.Provider, "model", modelCfg.Model)
			continue
		}

		if resolved := store.ResolveModelAlias(ctx, modelCfg.Provider, modelCfg.Model); resolved != modelCfg.Model {
			modelCfg.DisplayModel = modelCfg.Model
			modelCfg.Model = resolved
			cfg.Models[name] = modelCfg
		}

		// Resolve model aliases in routing rules
		for i, rule := range modelCfg.Routing {
			if provider, model, ok := strings.Cut(rule.Model, "/"); ok {
				if resolved := store.ResolveModelAlias(ctx, provider, model); resolved != model {
					modelCfg.Routing[i].Model = provider + "/" + resolved
				}
			}
		}
		cfg.Models[name] = modelCfg
	}
}

func isInlineModelEntry(name string, modelCfg latest.ModelConfig) bool {
	return name == modelCfg.Provider+"/"+modelCfg.Model
}
