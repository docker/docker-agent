package config

import (
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/userconfig"
)

// WithUserConfig merges global user-level definitions and settings from userCfg
// into the loaded agent config. Global definitions (providers, models,
// evaluators, mcps) are merged before validation so agent configs can reference
// them directly; global settings are applied after validation so all resolved
// agents, models, and toolsets receive them uniformly.
func WithUserConfig(userCfg *userconfig.Config) LoadOption {
	return func(o *loadOptions) {
		o.userConfig = userCfg
	}
}

func mergeUserConfigDefinitions(cfg *latest.Config, userCfg *userconfig.Config) {
	if cfg == nil || userCfg == nil {
		return
	}
	MergeGlobalProviders(cfg, userCfg.GetProviders())
}

func applyUserConfigSettings(cfg *latest.Config, userCfg *userconfig.Config) {
	if cfg == nil || userCfg == nil || userCfg.Settings == nil {
		return
	}
	settings := userCfg.Settings

	if settings.TitleModel != "" {
		for name, m := range cfg.Models {
			m.TitleModel = settings.TitleModel
			cfg.Models[name] = m
		}
	}
}
