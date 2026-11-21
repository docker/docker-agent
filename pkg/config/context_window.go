package config

import (
	"fmt"
	"maps"
	"slices"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider"
)

// ValidateContextWindows checks provider-dependent declarations after migration,
// global provider merging and model resolution, using the factory's API type.
func ValidateContextWindows(cfg *latest.Config) error {
	models := contextWindowModels(cfg)
	for _, name := range slices.Sorted(maps.Keys(models)) {
		m := models[name]
		if err := provider.ValidateContextWindow(&m, cfg.Providers); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

// contextWindowModels includes effective inline compaction models, which are not
// materialized in cfg.Models before the teamloader constructs their providers.
func contextWindowModels(cfg *latest.Config) map[string]latest.ModelConfig {
	models := make(map[string]latest.ModelConfig, len(cfg.Models))
	for name, m := range cfg.Models {
		models["models."+name] = m
	}
	for _, a := range cfg.Agents {
		ref := EffectiveCompactionModelRef(cfg, &a)
		if _, exists := cfg.Models[ref]; exists || ref == "" {
			continue
		}
		if m, err := latest.ParseModelRef(ref); err == nil {
			models["agents."+a.Name+".compaction_model"] = m
		}
	}
	return models
}
