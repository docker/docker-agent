package config

import (
	"maps"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
)

func TestApplyModelOverrides_InheritsParallelToolCalls(t *testing.T) {
	boolPtr := func(value bool) *bool { return &value }
	tests := []struct {
		name       string
		agents     []latest.AgentConfig
		models     map[string]latest.ModelConfig
		overrides  []string
		wantModels map[string]string
		wantValues map[string]*bool
	}{
		{
			name:       "inherits false into inline target",
			agents:     []latest.AgentConfig{{Name: "root", Model: "configured"}},
			models:     map[string]latest.ModelConfig{"configured": {Provider: "openai", Model: "configured", ParallelToolCalls: boolPtr(false)}},
			overrides:  []string{"openai/replacement"},
			wantModels: map[string]string{"root": "openai/replacement"},
			wantValues: map[string]*bool{"root": boolPtr(false)},
		},
		{
			name:       "inherits true into inline target",
			agents:     []latest.AgentConfig{{Name: "root", Model: "configured"}},
			models:     map[string]latest.ModelConfig{"configured": {Provider: "openai", Model: "configured", ParallelToolCalls: boolPtr(true)}},
			overrides:  []string{"openai/replacement"},
			wantModels: map[string]string{"root": "openai/replacement"},
			wantValues: map[string]*bool{"root": boolPtr(true)},
		},
		{
			name:       "named target explicit value wins",
			agents:     []latest.AgentConfig{{Name: "root", Model: "configured"}},
			models:     map[string]latest.ModelConfig{"configured": {Provider: "openai", Model: "configured", ParallelToolCalls: boolPtr(false)}, "replacement": {Provider: "openai", Model: "replacement", ParallelToolCalls: boolPtr(true)}},
			overrides:  []string{"replacement"},
			wantModels: map[string]string{"root": "replacement"},
			wantValues: map[string]*bool{"root": boolPtr(true)},
		},
		{
			name:       "named target omitted value wins",
			agents:     []latest.AgentConfig{{Name: "root", Model: "configured"}},
			models:     map[string]latest.ModelConfig{"configured": {Provider: "openai", Model: "configured", ParallelToolCalls: boolPtr(false)}, "replacement": {Provider: "openai", Model: "replacement"}},
			overrides:  []string{"replacement"},
			wantModels: map[string]string{"root": "replacement"},
			wantValues: map[string]*bool{"root": nil},
		},
		{
			name:       "source omitted does not inherit",
			agents:     []latest.AgentConfig{{Name: "root", Model: "configured"}},
			models:     map[string]latest.ModelConfig{"configured": {Provider: "openai", Model: "configured"}},
			overrides:  []string{"openai/replacement"},
			wantModels: map[string]string{"root": "openai/replacement"},
			wantValues: map[string]*bool{"root": nil},
		},
		{
			name: "shared target isolates per-agent policy",
			agents: []latest.AgentConfig{
				{Name: "root", Model: "serial"}, {Name: "worker", Model: "parallel"}, {Name: "unset", Model: "unset"},
			},
			models: map[string]latest.ModelConfig{
				"serial":   {Provider: "openai", Model: "serial", ParallelToolCalls: boolPtr(false)},
				"parallel": {Provider: "openai", Model: "parallel", ParallelToolCalls: boolPtr(true)},
				"unset":    {Provider: "openai", Model: "unset"},
			},
			overrides:  []string{"openai/replacement"},
			wantModels: map[string]string{"root": "openai/replacement", "worker": "openai/replacement", "unset": "openai/replacement"},
			wantValues: map[string]*bool{"root": boolPtr(false), "worker": boolPtr(true), "unset": nil},
		},
		{
			name: "excludes harness router alloy selector unresolved and providerless sources",
			agents: []latest.AgentConfig{
				{Name: "harness", Harness: &latest.HarnessConfig{Type: "codex"}},
				{Name: "router", Model: "router"},
				{Name: "alloy", Model: "alloy"},
				{Name: "selector", Model: "selector"},
				{Name: "missing", Model: "missing"},
				{Name: "providerless", Model: "providerless"},
				{Name: "csv", Model: "csv"},
			},
			models: map[string]latest.ModelConfig{
				"router":       {Provider: "openai", Routing: []latest.RoutingRule{{Model: "leaf"}}, ParallelToolCalls: boolPtr(false)},
				"alloy":        {Model: "leaf,leaf2", ParallelToolCalls: boolPtr(false)},
				"selector":     {Provider: "openai", FirstAvailable: []string{"leaf"}, ParallelToolCalls: boolPtr(false)},
				"providerless": {Model: "leaf", ParallelToolCalls: boolPtr(false)},
				"csv":          {Provider: "openai", Model: "leaf,leaf2", ParallelToolCalls: boolPtr(false)},
				"leaf":         {Provider: "openai", Model: "leaf"}, "leaf2": {Provider: "openai", Model: "leaf2"},
			},
			overrides: []string{"openai/replacement"},
			wantModels: map[string]string{
				"harness": "openai/replacement", "router": "openai/replacement", "alloy": "openai/replacement",
				"selector": "openai/replacement", "missing": "openai/replacement", "providerless": "openai/replacement", "csv": "openai/replacement",
			},
			wantValues: map[string]*bool{"harness": nil, "router": nil, "alloy": nil, "selector": nil, "missing": nil, "providerless": nil, "csv": nil},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &latest.Config{Agents: slices.Clone(tt.agents), Models: maps.Clone(tt.models)}
			require.NoError(t, ApplyModelOverrides(cfg, tt.overrides))
			expectedModelCount := len(tt.models)
			for _, modelRef := range tt.wantModels {
				if _, existed := tt.models[modelRef]; !existed {
					expectedModelCount++
					break
				}
			}
			assert.Len(t, cfg.Models, expectedModelCount)
			for name, original := range tt.models {
				assert.Equal(t, original, cfg.Models[name], "pre-existing model %q mutated", name)
			}
			for _, agent := range cfg.Agents {
				assert.Equal(t, tt.wantModels[agent.Name], agent.Model)
				model := cfg.Models[agent.Model]
				ApplyModelOverridePolicy(cfg, agent.Name, agent.Model, &model)
				want := tt.wantValues[agent.Name]
				if want == nil {
					assert.Nil(t, model.ParallelToolCalls)
				} else if assert.NotNil(t, model.ParallelToolCalls) {
					assert.Equal(t, *want, *model.ParallelToolCalls)
				}
			}
			for name := range cfg.Models {
				assert.NotContains(t, name, "__cli_model_")
			}
		})
	}
}

func TestApplyModelOverrides_RepeatedSameOverrideIsIdempotent(t *testing.T) {
	falseValue := false
	cfg := &latest.Config{
		Agents: []latest.AgentConfig{{Name: "root", Model: "configured"}},
		Models: map[string]latest.ModelConfig{
			"configured": {Provider: "openai", Model: "configured", ParallelToolCalls: &falseValue},
		},
	}

	require.NoError(t, ApplyModelOverrides(cfg, []string{"openai/first"}))
	firstState := cfg.InternalModelOverrideState()
	modelCount := len(cfg.Models)

	require.NoError(t, ApplyModelOverrides(cfg, []string{"openai/first"}))
	assert.Same(t, firstState, cfg.InternalModelOverrideState())
	assert.Equal(t, "openai/first", cfg.Agents[0].Model)
	assert.Len(t, cfg.Models, modelCount)
	model := cfg.Models["openai/first"]
	ApplyModelOverridePolicy(cfg, "root", "openai/first", &model)
	require.NotNil(t, model.ParallelToolCalls)
	assert.False(t, *model.ParallelToolCalls)
}

func TestApplyModelOverrides_FreshInvocationAuthorityAndDrift(t *testing.T) {
	falseValue := false
	trueValue := true
	cfg := &latest.Config{
		Agents: []latest.AgentConfig{{Name: "root", Model: "configured"}},
		Models: map[string]latest.ModelConfig{
			"configured": {Provider: "openai", Model: "configured", ParallelToolCalls: &falseValue},
		},
	}

	require.NoError(t, ApplyModelOverrides(cfg, []string{"openai/first"}))
	assert.Equal(t, "openai/first", cfg.Agents[0].Model)

	// A raw target created by the previous call is authoritative on a changed call.
	require.NoError(t, ApplyModelOverrides(cfg, []string{"root=openai/first"}))
	model := cfg.Models["openai/first"]
	ApplyModelOverridePolicy(cfg, "root", "openai/first", &model)
	assert.Nil(t, model.ParallelToolCalls)

	// A target added after the first call is authoritative on the next call.
	cfg.Models["named"] = latest.ModelConfig{Provider: "openai", Model: "named"}
	require.NoError(t, ApplyModelOverrides(cfg, []string{"named"}))
	model = cfg.Models["named"]
	ApplyModelOverridePolicy(cfg, "root", "named", &model)
	assert.Nil(t, model.ParallelToolCalls)

	// Unchanged expected post-state keeps the original source receipt.
	require.NoError(t, ApplyModelOverrides(cfg, []string{"openai/second"}))
	model = cfg.Models["openai/second"]
	ApplyModelOverridePolicy(cfg, "root", "openai/second", &model)
	require.NotNil(t, model.ParallelToolCalls)
	assert.False(t, *model.ParallelToolCalls)

	// External drift invalidates the receipt and snapshots the current source.
	drifted := cfg.Models["openai/second"]
	drifted.ParallelToolCalls = &trueValue
	cfg.Models["openai/second"] = drifted
	require.NoError(t, ApplyModelOverrides(cfg, []string{"openai/third"}))
	model = cfg.Models["openai/third"]
	ApplyModelOverridePolicy(cfg, "root", "openai/third", &model)
	require.NotNil(t, model.ParallelToolCalls)
	assert.True(t, *model.ParallelToolCalls)
}

func TestApplyModelOverridePolicyRequiresAgentAndRealRef(t *testing.T) {
	falseValue := false
	cfg := &latest.Config{
		Agents: []latest.AgentConfig{{Name: "root", Model: "configured"}},
		Models: map[string]latest.ModelConfig{
			"configured": {Provider: "openai", Model: "configured", ParallelToolCalls: &falseValue},
		},
	}
	require.NoError(t, ApplyModelOverrides(cfg, []string{"openai/replacement"}))

	for _, tc := range []struct {
		agent, ref string
		wantNil    bool
	}{
		{agent: "root", ref: "openai/replacement"},
		{agent: "other", ref: "openai/replacement", wantNil: true},
		{agent: "root", ref: "openai/other", wantNil: true},
	} {
		model := latest.ModelConfig{Provider: "openai", Model: "replacement"}
		ApplyModelOverridePolicy(cfg, tc.agent, tc.ref, &model)
		if tc.wantNil {
			assert.Nil(t, model.ParallelToolCalls)
		} else if assert.NotNil(t, model.ParallelToolCalls) {
			assert.False(t, *model.ParallelToolCalls)
		}
	}
}

func TestIsConcreteModelConfigRequiresProviderAndModel(t *testing.T) {
	t.Parallel()

	assert.True(t, isConcreteModelConfig(latest.ModelConfig{Provider: "openai", Model: "gpt-5"}))
	assert.False(t, isConcreteModelConfig(latest.ModelConfig{Provider: "openai"}))
	assert.False(t, isConcreteModelConfig(latest.ModelConfig{Model: "gpt-5"}))
}

func TestApplyModelOverrides_NoOverridesPreservesNilModels(t *testing.T) {
	cfg := &latest.Config{}
	require.NoError(t, ApplyModelOverrides(cfg, nil))
	assert.Nil(t, cfg.Models)
}
