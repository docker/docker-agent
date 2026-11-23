package config

import (
	"strings"

	"github.com/docker/docker-agent/pkg/config/latest"
)

// EffectiveCompactionModelRef resolves the compaction-model reference for an
// agent: the agent-level `compaction_model` wins, then the `compaction_model`
// of the first of the agent's configured models that sets it, then the
// provider-level default of the first of those models whose provider sets
// one. It returns "" when none is set (compaction then reuses the agent's own
// model). The reference may be a named model from the models section or an
// inline "provider/model" spec.
//
// This is the single source of truth for compaction-model precedence: the
// teamloader uses it to instantiate the model, and the config pipeline uses it
// for `first_available` reachability and required-credential preflight.
func EffectiveCompactionModelRef(cfg *latest.Config, a *latest.AgentConfig) string {
	if a.CompactionModel != "" {
		return a.CompactionModel
	}
	for name := range strings.SplitSeq(a.Model, ",") {
		if modelCfg, ok := cfg.Models[name]; ok && modelCfg.CompactionModel != "" {
			return modelCfg.CompactionModel
		}
	}
	for name := range strings.SplitSeq(a.Model, ",") {
		modelCfg, ok := cfg.Models[name]
		if !ok {
			continue
		}
		if providerCfg, ok := cfg.Providers[modelCfg.Provider]; ok && providerCfg.CompactionModel != "" {
			return providerCfg.CompactionModel
		}
	}
	for name := range strings.SplitSeq(a.Model, ",") {
		model := cfg.Models[strings.TrimSpace(name)]
		if model.IsFirstAvailable() {
			return ""
		}
	}
	return cfg.DefaultCompactionModel
}
