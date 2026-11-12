package config

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/config/latest"
)

type modelOverrideReceipt struct {
	sourceModel       string
	sourcePolicy      *bool
	eligible          bool
	requestedModel    string
	expectedPostModel string
	expectedPostValue latest.ModelConfig
}

type modelOverridePolicy struct {
	modelRef          string
	parallelToolCalls *bool
}

type modelOverrideState struct {
	receipts      map[string]modelOverrideReceipt
	policies      map[string]modelOverridePolicy
	lastOverrides []string
}

func overrideState(cfg *latest.Config) *modelOverrideState {
	state, _ := cfg.InternalModelOverrideState().(*modelOverrideState)
	if state == nil {
		state = &modelOverrideState{
			receipts: make(map[string]modelOverrideReceipt),
			policies: make(map[string]modelOverridePolicy),
		}
		cfg.SetInternalModelOverrideState(state)
	}
	return state
}

func cloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	return new(*value)
}

func isConcreteModelConfig(model latest.ModelConfig) bool {
	return model.Provider != "" && model.Model != "" && !strings.Contains(model.Model, ",") &&
		len(model.Routing) == 0 && !model.IsFirstAvailable()
}

func sourceReceipt(cfg *latest.Config, state *modelOverrideState, agent latest.AgentConfig) modelOverrideReceipt {
	if receipt, ok := state.receipts[agent.Name]; ok {
		current, exists := cfg.Models[agent.Model]
		if agent.Model == receipt.expectedPostModel && exists && reflect.DeepEqual(current, receipt.expectedPostValue) {
			return receipt
		}
		delete(state.receipts, agent.Name)
	}

	model, exists := cfg.Models[agent.Model]
	return modelOverrideReceipt{
		sourceModel:  agent.Model,
		sourcePolicy: cloneBool(model.ParallelToolCalls),
		eligible:     agent.Harness == nil && exists && isConcreteModelConfig(model),
	}
}

// ApplyModelOverridePolicy applies an inherited per-agent CLI override policy
// to a copied model config. The manifest model map remains authoritative and
// unmodified.
func ApplyModelOverridePolicy(cfg *latest.Config, agentName, modelRef string, model *latest.ModelConfig) {
	if cfg == nil || model == nil {
		return
	}
	state, _ := cfg.InternalModelOverrideState().(*modelOverrideState)
	if state == nil {
		return
	}
	policy, ok := state.policies[agentName]
	if !ok || policy.modelRef != modelRef {
		return
	}
	model.ParallelToolCalls = cloneBool(policy.parallelToolCalls)
}

// ApplyModelOverrides applies CLI model overrides to the configuration.
func ApplyModelOverrides(cfg *latest.Config, overrides []string) error {
	if len(overrides) == 0 {
		return nil
	}

	state := overrideState(cfg)
	if slices.Equal(overrides, state.lastOverrides) && receiptsIntact(cfg, state) {
		return nil
	}
	invocationTargets := make(map[string]struct{}, len(cfg.Models))
	for name := range cfg.Models {
		invocationTargets[name] = struct{}{}
	}
	receipts := make(map[string]modelOverrideReceipt, len(cfg.Agents))
	for _, agent := range cfg.Agents {
		receipts[agent.Name] = sourceReceipt(cfg, state, agent)
	}

	for _, override := range overrides {
		if err := applySingleOverride(cfg, override); err != nil {
			return err
		}
	}

	if err := ensureModelsExist(cfg); err != nil {
		return err
	}
	policies := make(map[string]modelOverridePolicy)
	for _, agent := range cfg.Agents {
		receipt := receipts[agent.Name]
		requestedRef := agent.Model
		target := cfg.Models[requestedRef]
		_, targetExisted := invocationTargets[requestedRef]
		if receipt.eligible && receipt.sourcePolicy != nil && !targetExisted {
			policies[agent.Name] = modelOverridePolicy{
				modelRef:          requestedRef,
				parallelToolCalls: cloneBool(receipt.sourcePolicy),
			}
		}
		receipt.requestedModel = requestedRef
		receipt.expectedPostModel = requestedRef
		receipt.expectedPostValue = target
		state.receipts[agent.Name] = receipt
	}
	state.policies = policies
	state.lastOverrides = slices.Clone(overrides)
	return nil
}

func receiptsIntact(cfg *latest.Config, state *modelOverrideState) bool {
	for _, agent := range cfg.Agents {
		receipt, ok := state.receipts[agent.Name]
		if !ok || agent.Model != receipt.expectedPostModel {
			return false
		}
		model, exists := cfg.Models[agent.Model]
		if !exists || !reflect.DeepEqual(model, receipt.expectedPostValue) {
			return false
		}
	}
	return len(state.receipts) == len(cfg.Agents)
}

// applySingleOverride processes a single model override string.
func applySingleOverride(cfg *latest.Config, override string) error {
	override = strings.TrimSpace(override)
	if override == "" {
		return nil
	}

	if strings.Contains(override, ",") {
		for part := range strings.SplitSeq(override, ",") {
			if err := applySingleOverride(cfg, part); err != nil {
				return err
			}
		}
		return nil
	}

	agentName, modelSpec, ok := strings.Cut(override, "=")
	if ok {
		agentName = strings.TrimSpace(agentName)
		if agentName == "" {
			return fmt.Errorf("empty agent name in override: %s", override)
		}

		modelSpec = strings.TrimSpace(modelSpec)
		if modelSpec == "" {
			return fmt.Errorf("empty model specification in override: %s", override)
		}

		ok := cfg.Agents.Update(agentName, func(a *latest.AgentConfig) {
			a.Model = modelSpec
		})
		if !ok {
			return fmt.Errorf("unknown agent '%s'", agentName)
		}
	} else {
		modelSpec := strings.TrimSpace(override)
		if modelSpec == "" {
			return errors.New("empty model specification")
		}

		for _, agent := range cfg.Agents {
			cfg.Agents.Update(agent.Name, func(a *latest.AgentConfig) {
				a.Model = modelSpec
			})
		}
	}

	return nil
}

// ensureModelsExist ensures that all models referenced by agents exist in cfg.Models
// This handles inline model specs that may have been added via CLI overrides
func ensureModelsExist(cfg *latest.Config) error {
	if cfg.Models == nil {
		cfg.Models = map[string]latest.ModelConfig{}
	}

	// Expand alloy model compositions in agent model references and ensure resulting
	// referenced models exist.
	for _, agent := range cfg.Agents {
		if agent.Harness != nil {
			continue
		}

		expandedModel, err := expandAlloyModelRef(cfg, agent.Model)
		if err != nil {
			return fmt.Errorf("agent '%s': %w", agent.Name, err)
		}

		cfg.Agents.Update(agent.Name, func(a *latest.AgentConfig) {
			a.Model = expandedModel
		})

		for modelName := range strings.SplitSeq(expandedModel, ",") {
			if err := ensureSingleModelExists(cfg, modelName, fmt.Sprintf("agent '%s'", agent.Name)); err != nil {
				return err
			}
		}
	}

	// Ensure models referenced by routing rules exist
	for modelName, modelCfg := range cfg.Models {
		for i, rule := range modelCfg.Routing {
			if err := ensureSingleModelExists(cfg, rule.Model, fmt.Sprintf("routing rule %d in model '%s'", i, modelName)); err != nil {
				return err
			}
		}
	}

	// Ensure models referenced by RAG strategies exist
	for ragName, ragToolset := range cfg.RAG {
		if ragToolset.RAGConfig == nil {
			continue
		}
		for _, stratCfg := range ragToolset.RAGConfig.Strategies {
			rawModel, ok := stratCfg.Params["model"]
			if !ok {
				continue
			}

			modelName, ok := rawModel.(string)
			if !ok {
				return fmt.Errorf("RAG strategy '%s' in RAG '%s' has non-string model value", stratCfg.Type, ragName)
			}

			if err := ensureSingleModelExists(cfg, modelName, fmt.Sprintf("RAG strategy '%s' in RAG '%s'", stratCfg.Type, ragName)); err != nil {
				return err
			}
		}
	}

	return nil
}

func isAlloyModelConfig(cfg latest.ModelConfig) bool {
	return cfg.Provider == "" && strings.Contains(cfg.Model, ",")
}

// expandAlloyModelRef expands a model reference if it points to an alloy model.
// It also expands already-comma-separated model refs by expanding each part.
func expandAlloyModelRef(cfg *latest.Config, modelRef string) (string, error) {
	modelRef = strings.TrimSpace(modelRef)
	if modelRef == "" {
		return "", nil
	}

	// Fast path for non-compositions.
	if !strings.Contains(modelRef, ",") {
		modelCfg, exists := cfg.Models[modelRef]
		if !exists || !isAlloyModelConfig(modelCfg) {
			return modelRef, nil
		}
		return expandAlloyModelRef(cfg, modelCfg.Model)
	}

	var expanded []string
	for part := range strings.SplitSeq(modelRef, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		exp, err := expandAlloyModelRef(cfg, part)
		if err != nil {
			return "", err
		}
		if exp == "" {
			continue
		}
		expanded = append(expanded, exp)
	}

	return strings.Join(expanded, ","), nil
}

// ensureSingleModelExists normalizes shorthand model IDs like "openai/gpt-5-mini"
// into full entries in cfg.Models so they can be reused by agents, RAG, and other
// subsystems without duplicating parsing logic.
func ensureSingleModelExists(cfg *latest.Config, modelName, context string) error {
	modelName = strings.TrimSpace(modelName)
	if modelName == "" || modelName == "auto" {
		// "auto" is handled dynamically at runtime and does not need a config entry.
		return nil
	}

	if _, exists := cfg.Models[modelName]; exists {
		return nil
	}

	parsed, err := latest.ParseModelRef(modelName)
	if err != nil {
		return fmt.Errorf("%s references non-existent model '%s'", context, modelName)
	}

	cfg.Models[modelName] = parsed

	return nil
}
