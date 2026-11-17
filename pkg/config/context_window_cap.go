package config

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

// ValidateContextWindowCaps rejects a model-level context_window above the context
// window the models.dev catalogue declares for the same model. It runs after
// alias resolution. Environment-dependent catalogue identities are checked at
// construction instead, so unused optional candidates need not resolve.
// It compares the raw catalogue limit rather than the resolved window, which
// already includes the override.
// A model the catalogue has no limit for accepts any positive value; nothing
// is capped by guess.
func ValidateContextWindowCaps(ctx context.Context, cfg *latest.Config, store *modelsdev.Store, env environment.Provider) error {
	for _, name := range slices.Sorted(maps.Keys(cfg.Models)) {
		m := cfg.Models[name]
		// Provider defaults do not change DisplayOrModel. Defer only that lookup
		// identity, not unrelated runtime fields such as the endpoint.
		if store != nil && len(environment.Refs(m.DisplayOrModel())) > 0 {
			continue
		}
		if err := provider.ValidateContextWindowCap(ctx, &m, cfg.Providers, store, env); err != nil {
			return fmt.Errorf("models.%s: %w", name, err)
		}
	}
	return nil
}
