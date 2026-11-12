package provider

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

func TestCloneRoutedConfigurationsAreIsolated(t *testing.T) {
	t.Parallel()
	var children []*instrumentTestProvider
	registry := NewRegistry(map[string]Factory{"test": func(_ context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
		p := &instrumentTestProvider{Config: base.Config{ModelConfig: *cfg, ModelOptions: options.Apply(opts...), Env: env}}
		if cfg.Model == "child" {
			children = append(children, p)
		}
		return p, nil
	}})
	models := map[string]latest.ModelConfig{
		"target": {Provider: "test", Model: "child", Name: "named", DisplayModel: "display", ThinkingBudget: &latest.ThinkingBudget{Tokens: 8192}, MaxTokens: new(int64(16384)), ProviderOpts: map[string]any{"nested": map[string]any{"value": "initial"}}},
		"unused": {ProviderOpts: map[string]any{"bad": make(chan int)}},
	}
	cfg := &latest.ModelConfig{Provider: "test", Model: "fallback", Routing: []latest.RoutingRule{{Model: "target", Examples: []string{"hello"}}}}
	original, err := registry.NewWithModels(t.Context(), cfg, models, environment.NewNoEnvProvider())
	require.NoError(t, err)
	models["target"].ProviderOpts["nested"].(map[string]any)["value"] = "input changed"
	models["target"].ThinkingBudget.Tokens = 1
	first, err := Clone(t.Context(), original, options.WithMaxTokens(128))
	require.NoError(t, err)
	second, err := Clone(t.Context(), first)
	require.NoError(t, err)
	require.NotNil(t, second)
	require.Len(t, children, 3)
	for i, child := range children {
		assert.Equal(t, "initial", child.ModelConfig.ProviderOpts["nested"].(map[string]any)["value"])
		assert.Equal(t, 8192, child.ModelConfig.ThinkingBudget.Tokens)
		if i == 0 {
			assert.Equal(t, int64(16384), *child.ModelConfig.MaxTokens)
		} else {
			assert.Equal(t, int64(128), *child.ModelConfig.MaxTokens)
		}
		assert.Equal(t, "named", child.ModelConfig.Name)
		assert.Equal(t, "display", child.ModelConfig.DisplayModel)
	}
	for i, child := range children {
		child.ModelConfig.ProviderOpts["nested"].(map[string]any)["value"] = i
		child.ModelConfig.ThinkingBudget.Tokens = i
		*child.ModelConfig.MaxTokens = int64(i)
	}
	for i, child := range children {
		assert.Equal(t, i, child.ModelConfig.ProviderOpts["nested"].(map[string]any)["value"])
		assert.Equal(t, i, child.ModelConfig.ThinkingBudget.Tokens)
		assert.Equal(t, int64(i), *child.ModelConfig.MaxTokens)
	}
	assert.Equal(t, "input changed", models["target"].ProviderOpts["nested"].(map[string]any)["value"])
	assert.Equal(t, 1, models["target"].ThinkingBudget.Tokens)
	models["target"] = latest.ModelConfig{Provider: "test", Model: "replacement"}
	_, err = Clone(t.Context(), second)
	require.NoError(t, err)
	require.Len(t, children, 4)
	assert.Equal(t, "initial", children[3].ModelConfig.ProviderOpts["nested"].(map[string]any)["value"])
	assert.Equal(t, 8192, children[3].ModelConfig.ThinkingBudget.Tokens)
}

func TestCloneRejectsUncopyableRoutedChild(t *testing.T) {
	t.Parallel()
	calls := 0
	registry := NewRegistry(map[string]Factory{"test": func(context.Context, *latest.ModelConfig, environment.Provider, ...options.Opt) (Provider, error) {
		calls++
		return &instrumentTestProvider{}, nil
	}})
	cfg := latest.ModelConfig{Provider: "test", Model: "fallback", Routing: []latest.RoutingRule{{Model: "target", Examples: []string{"hello"}}}}
	models := map[string]latest.ModelConfig{"target": {Provider: "test", Model: "child", ProviderOpts: map[string]any{"bad": make(chan int)}}}
	p, err := registry.NewWithModels(t.Context(), &cfg, models, environment.NewNoEnvProvider())
	require.ErrorIs(t, err, ErrUncopyableConfig)
	require.ErrorContains(t, err, `routed model "target"`)
	assert.Nil(t, p)
	assert.Zero(t, calls)
	original := &instrumentTestProvider{Config: base.Config{ModelConfig: cfg}}
	registry.attachRebuilder(original, models, environment.NewNoEnvProvider())
	clone, err := Clone(t.Context(), original)
	require.ErrorIs(t, err, ErrUncopyableConfig)
	require.ErrorContains(t, err, `routed model "target"`)
	assert.Nil(t, clone)
	assert.Zero(t, calls)
}
