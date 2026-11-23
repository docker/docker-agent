package config

import (
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/goccy/go-yaml"

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

// Historical schemas validate authentication during decoding. Seed only provider
// identities they understand; merge full current definitions after migration.
func mergeUserProviderTypes(data []byte, version string, userCfg *userconfig.Config) ([]byte, error) {
	v, err := strconv.Atoi(version)
	if err != nil || v < 8 || len(userCfg.GetProviders()) == 0 {
		return data, nil
	}
	var doc yaml.MapSlice
	if err := yaml.UnmarshalWithOptions(data, &doc, yaml.UseOrderedMap()); err != nil {
		return nil, err
	}
	value, hasProviders := lookupKey(doc, "providers")
	providers, ok := value.(yaml.MapSlice)
	if hasProviders && value != nil && !ok {
		return data, nil // Let the historical parser report malformed providers.
	}
	global := userCfg.GetProviders()
	for _, name := range slices.Sorted(maps.Keys(global)) {
		p := global[name]
		if _, exists := lookupKey(providers, name); !exists {
			providers = append(providers, yaml.MapItem{Key: name, Value: yaml.MapSlice{{Key: "provider", Value: p.Provider}}})
		}
	}
	if hasProviders {
		for i := range doc {
			if doc[i].Key == "providers" {
				doc[i].Value = providers
			}
		}
	} else {
		doc = append(doc, yaml.MapItem{Key: "providers", Value: providers})
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("merging user provider identities: %w", err)
	}
	return out, nil
}

func mergeUserConfigDefinitions(cfg *latest.Config, userCfg *userconfig.Config) {
	if cfg == nil || userCfg == nil {
		return
	}
	MergeGlobalProviders(cfg, userCfg.GetProviders())
	mergeGlobalModels(cfg, userCfg.GetModels())
	mergeGlobalEvaluators(cfg, userCfg.GetEvaluators())
}

func mergeGlobalModels(cfg *latest.Config, global map[string]latest.ModelConfig) {
	for name, modelCfg := range global {
		if _, exists := cfg.Models[name]; exists {
			continue
		}
		if strings.TrimSpace(name) == "" {
			slog.Warn("Ignoring model with empty name from user config")
			continue
		}
		if cfg.Models == nil {
			cfg.Models = map[string]latest.ModelConfig{}
		}
		cfg.Models[name] = modelCfg
	}
}

func mergeGlobalEvaluators(cfg *latest.Config, global map[string]latest.EvaluatorConfig) {
	for name, def := range global {
		if _, exists := cfg.Evaluators[name]; exists {
			continue
		}
		if strings.TrimSpace(name) == "" {
			slog.Warn("Ignoring evaluator with empty name from user config")
			continue
		}
		if err := def.Validate(); err != nil {
			slog.Warn("Ignoring invalid evaluator from user config", "evaluator", name, "error", err)
			continue
		}
		if cfg.Evaluators == nil {
			cfg.Evaluators = map[string]latest.EvaluatorConfig{}
		}
		cfg.Evaluators[name] = def
	}
}

func applyUserConfigSettings(cfg *latest.Config, userCfg *userconfig.Config) {
	if cfg == nil || userCfg == nil || userCfg.Settings == nil {
		return
	}
	settings := userCfg.Settings

	if settings.TitleModel != "" {
		cfg.DefaultTitleModel = settings.TitleModel
	}
	if settings.CompactionModel != "" {
		cfg.DefaultCompactionModel = settings.CompactionModel
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

	if settings.StreamIdleTimeout != nil {
		rt := ensureRuntime(cfg)
		if rt.StreamIdleTimeout == nil {
			rt.StreamIdleTimeout = new(*settings.StreamIdleTimeout)
		}
	}

	if !settings.Budget.IsZero() {
		cfg.Budget = mergeBudgetCeiling(cfg.Budget, settings.Budget)
	}

	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		if settings.SessionCompaction != nil && a.SessionCompaction == nil {
			a.SessionCompaction = new(*settings.SessionCompaction)
		}
		if settings.CompactionThreshold != nil && a.CompactionThreshold == nil {
			a.CompactionThreshold = new(*settings.CompactionThreshold)
		}
		if settings.MaxIterations > 0 && a.MaxIterations == 0 {
			a.MaxIterations = settings.MaxIterations
		}
		if settings.MaxConsecutiveToolCalls > 0 && a.MaxConsecutiveToolCalls == 0 {
			a.MaxConsecutiveToolCalls = settings.MaxConsecutiveToolCalls
		}
		if settings.MaxToolResultTokens > 0 && a.MaxToolResultTokens == 0 {
			a.MaxToolResultTokens = settings.MaxToolResultTokens
		}
		if settings.MaxOldToolCallTokens > 0 && a.MaxOldToolCallTokens == 0 {
			a.MaxOldToolCallTokens = settings.MaxOldToolCallTokens
		}
		if settings.NumHistoryItems > 0 && a.NumHistoryItems == 0 {
			a.NumHistoryItems = settings.NumHistoryItems
		}
		if settings.CodeModeTools {
			a.CodeModeTools = true
		}
	}
}

func ensureRuntime(cfg *latest.Config) *latest.RuntimeDefaults {
	if cfg.Runtime == nil {
		cfg.Runtime = &latest.RuntimeDefaults{}
	}
	return cfg.Runtime
}

func mergeBudgetCeiling(agentBudget, globalBudget *latest.BudgetConfig) *latest.BudgetConfig {
	merged := latest.BudgetConfig{}
	if agentBudget != nil {
		merged = *agentBudget
	}
	if globalBudget.MaxCost > 0 && !(merged.MaxCost > 0 && merged.MaxCost <= globalBudget.MaxCost) {
		merged.MaxCost = globalBudget.MaxCost
	}
	if globalBudget.MaxTokens > 0 && (merged.MaxTokens == 0 || globalBudget.MaxTokens < merged.MaxTokens) {
		merged.MaxTokens = globalBudget.MaxTokens
	}
	if globalBudget.MaxTime.Duration > 0 && (merged.MaxTime.Duration == 0 || globalBudget.MaxTime.Duration < merged.MaxTime.Duration) {
		merged.MaxTime = globalBudget.MaxTime
	}
	return &merged
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

// EffectiveTitleModelRef matches the first title model used by a model-backed agent.
func EffectiveTitleModelRef(cfg *latest.Config, agent *latest.AgentConfig) string {
	if agent.Harness != nil {
		return ""
	}
	if cfg.DefaultTitleModel != "" {
		return cfg.DefaultTitleModel
	}
	for name := range strings.SplitSeq(agent.Model, ",") {
		model := cfg.Models[strings.TrimSpace(name)]
		if model.IsFirstAvailable() {
			return ""
		}
		if model.TitleModel != "" {
			return model.TitleModel
		}
	}
	return cfg.DefaultTitleModel
}
