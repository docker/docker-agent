package provider

import (
	"context"
	"log/slog"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

// CloneWithOptions returns a new Provider instance using the same provider/model
// as the base provider, applying the provided options. If cloning fails, the
// original base provider is returned.
func CloneWithOptions(ctx context.Context, baseProvider Provider, opts ...options.Opt) Provider {
	cfg := baseProvider.BaseConfig()
	modelConfig, mergedOpts := mergeCloneOptions(cfg, opts)

	// Use NewWithModels to support cloning routers that reference other models.
	// cfg.Models is populated by routers; for other providers it's nil (which is fine).
	clone, err := NewWithModels(ctx, &modelConfig, cfg.Models, cfg.Env, mergedOpts...)
	if err != nil {
		slog.DebugContext(ctx, "Failed to clone provider; using base provider", "error", err, "id", baseProvider.ID())
		return baseProvider
	}

	return clone
}

// mergeCloneOptions is the pure half of CloneWithOptions. Given the base
// provider's configuration and the user-supplied overrides, it returns:
//
//   - a copy of the base ModelConfig with explicit overrides applied (currently
//     MaxTokens and the no-thinking flag), and
//   - the full ordered slice of options that should be passed to NewWithModels
//     (existing options first, then user overrides; later opts win).
//
// Splitting this out from the impure NewWithModels call lets us table-test the
// option-merging logic without spinning up an HTTP server.
func mergeCloneOptions(cfg base.Config, opts []options.Opt) (latest.ModelConfig, []options.Opt) {
	// Preserve existing options, then apply overrides. Later opts take precedence.
	baseOpts := options.FromModelOptions(cfg.ModelOptions)
	mergedOpts := append(baseOpts, opts...)

	// Apply every option to a single accumulator so we can read the final
	// effective values directly. "Later opt wins" semantics fall out naturally.
	var merged options.ModelOptions
	for _, opt := range mergedOpts {
		opt(&merged)
	}

	modelConfig := cfg.ModelConfig
	if mt := merged.MaxTokens(); mt != 0 {
		modelConfig.MaxTokens = &mt
	}
	if merged.NoThinking() {
		modelConfig.ThinkingBudget = nil
	}
	return modelConfig, mergedOpts
}
