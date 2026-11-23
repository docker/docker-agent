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

	if settings.RedactSecrets != nil {
		for i := range cfg.Agents {
			if *settings.RedactSecrets || cfg.Agents[i].RedactSecrets == nil {
				cfg.Agents[i].RedactSecrets = new(*settings.RedactSecrets)
			}
		}
	}

	if settings.SudoAskpass != nil {
		visitToolsets(cfg, func(ts *latest.Toolset) {
			if ts.Type != "shell" {
				return
			}
			if !*settings.SudoAskpass || ts.SudoAskpass == nil {
				ts.SudoAskpass = new(*settings.SudoAskpass)
			}
		})
	}

	if settings.Sandbox {
		ensureRuntime(cfg).Sandbox = true
	}
}

func ensureRuntime(cfg *latest.Config) *latest.RuntimeDefaults {
	if cfg.Runtime == nil {
		cfg.Runtime = &latest.RuntimeDefaults{}
	}
	return cfg.Runtime
}

func visitToolsets(cfg *latest.Config, fn func(*latest.Toolset)) {
	for i := range cfg.Agents {
		for j := range cfg.Agents[i].Toolsets {
			fn(&cfg.Agents[i].Toolsets[j])
		}
	}
	for name, ts := range cfg.Toolsets {
		fn(&ts)
		cfg.Toolsets[name] = ts
	}
}
