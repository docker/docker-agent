// Package bootstrap translates loaded agent configuration into runtime options.
// Callers retain ownership of session stores, interaction policy and lifetimes.
package bootstrap

import (
	"log/slog"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/runtime/modelcatalog"
	"github.com/docker/docker-agent/pkg/teamloader"
)

// RuntimeOpts carries model-switching configuration and budgets into a runtime.
// It resolves the shared environment and model store when called, not when applied.
func RuntimeOpts(loaded *teamloader.LoadResult, runConfig *config.RuntimeConfig) []runtime.Opt {
	modelSwitcherCfg := &modelcatalog.Config{
		Models:             loaded.Models,
		Providers:          loaded.Providers,
		ModelsGateway:      runConfig.ModelsGateway,
		EncryptedConfig:    loaded.EncryptedConfig,
		EnvProvider:        runConfig.EnvProvider(),
		ProviderRegistry:   loaded.ProviderRegistry,
		AgentDefaultModels: loaded.AgentDefaultModels,
	}
	// Reuse the loader's warmed catalog; a failed lookup keeps the runtime's lazy fallback.
	if store, err := runConfig.ModelsDevStore(); err == nil {
		modelSwitcherCfg.ModelsStore = store
	} else {
		slog.Warn("Failed to obtain shared models.dev store; runtime will use its own", "error", err)
	}
	return []runtime.Opt{
		runtime.WithModelSwitcherConfig(modelSwitcherCfg),
		runtime.WithModelsDevFallback(runConfig.ModelsDevFallback),
		runtime.WithBudget(loaded.Budget),
		runtime.WithNamedBudgets(loaded.Budgets, loaded.AgentBudgets),
	}
}
