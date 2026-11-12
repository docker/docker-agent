package provider

import (
	"context"
	"math"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/model/provider/providerutil"
)

func cloneNumericOptions() map[string]any {
	return map[string]any{
		"int64": int64(9007199254740993), "uint64": uint64(math.MaxUint64),
		"int8": int8(-7), "float32": float32(0.1), "top_k": uint64(40),
		"extra_body": map[string]any{"limit": int64(9007199254740993), "items": []any{int8(-7), float32(0.1)}},
	}
}

func TestClonePreservesNumericTypesCumulatively(t *testing.T) {
	t.Parallel()
	source := cloneNumericOptions()
	var p Provider = &instrumentTestProvider{Config: base.Config{ModelConfig: latest.ModelConfig{Provider: "test", Model: "base", ProviderOpts: source}}}
	var rebuild base.RebuildProviderFunc
	rebuild = func(_ context.Context, cfg *latest.ModelConfig, opts ...options.Opt) (Provider, error) {
		return &instrumentTestProvider{Config: base.Config{ModelConfig: *cfg, ModelOptions: options.Apply(opts...), RebuildProvider: rebuild}}, nil
	}
	p.(*instrumentTestProvider).RebuildProvider = rebuild
	for range 3 {
		clone, err := Clone(t.Context(), p)
		require.NoError(t, err)
		copied := clone.BaseConfig().ModelConfig.ProviderOpts
		require.Equal(t, source, copied)
		for key, value := range source {
			assert.IsType(t, value, copied[key])
		}
		baseTopK, baseOK := providerutil.GetProviderOptInt64(source, "top_k")
		cloneTopK, cloneOK := providerutil.GetProviderOptInt64(copied, "top_k")
		assert.Equal(t, baseTopK, cloneTopK)
		assert.Equal(t, baseOK, cloneOK)
		p = clone
	}
	copied := p.BaseConfig().ModelConfig.ProviderOpts
	copied["extra_body"].(map[string]any)["items"].([]any)[0] = int8(0)
	assert.Equal(t, int8(-7), source["extra_body"].(map[string]any)["items"].([]any)[0])
}

func TestClonePreservesYAMLNumericTypes(t *testing.T) {
	t.Parallel()
	var cfg latest.ModelConfig
	require.NoError(t, yaml.Unmarshal([]byte(`provider: test
model: base
provider_opts:
  big: 9007199254740993
  negative: -5
  fractional: 0.5
  date: 2026-01-01
  quoted_date: "2026-01-01"
  timestamp: 2026-01-01T10:00:00Z
  nested: {items: [9007199254740993, -5, 0.5]}
`), &cfg))
	require.IsType(t, uint64(0), cfg.ProviderOpts["big"])
	require.Equal(t, uint64(9007199254740993), cfg.ProviderOpts["big"])
	require.IsType(t, int64(0), cfg.ProviderOpts["negative"])
	require.IsType(t, float64(0), cfg.ProviderOpts["fractional"])
	for _, key := range []string{"date", "quoted_date", "timestamp"} {
		require.IsType(t, "", cfg.ProviderOpts[key])
	}
	registry := NewRegistry(map[string]Factory{"test": func(_ context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
		return &instrumentTestProvider{Config: base.Config{ModelConfig: *cfg, ModelOptions: options.Apply(opts...), Env: env}}, nil
	}})
	p, err := registry.New(t.Context(), &cfg, environment.NewNoEnvProvider())
	require.NoError(t, err)
	for range 3 {
		p, err = Clone(t.Context(), p)
		require.NoError(t, err)
		assert.Equal(t, cfg.ProviderOpts, p.BaseConfig().ModelConfig.ProviderOpts)
	}
}

func TestCloneRoutedPreservesNumericTypes(t *testing.T) {
	t.Parallel()
	var children []*instrumentTestProvider
	registry := NewRegistry(map[string]Factory{"test": func(_ context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
		p := &instrumentTestProvider{Config: base.Config{ModelConfig: *cfg, ModelOptions: options.Apply(opts...), Env: env}}
		if cfg.Model == "child" {
			children = append(children, p)
		}
		return p, nil
	}})
	source := cloneNumericOptions()
	p, err := registry.NewWithModels(t.Context(), &latest.ModelConfig{Provider: "test", Model: "fallback", Routing: []latest.RoutingRule{{Model: "target"}}}, map[string]latest.ModelConfig{"target": {Provider: "test", Model: "child", ProviderOpts: source}}, environment.NewNoEnvProvider())
	require.NoError(t, err)
	for range 3 {
		p, err = Clone(t.Context(), p)
		require.NoError(t, err)
	}
	require.Len(t, children, 4)
	for _, child := range children {
		assert.Equal(t, source, child.ModelConfig.ProviderOpts)
	}
	children[3].ModelConfig.ProviderOpts["extra_body"].(map[string]any)["limit"] = int64(0)
	assert.Equal(t, int64(9007199254740993), children[0].ModelConfig.ProviderOpts["extra_body"].(map[string]any)["limit"])
	assert.Equal(t, int64(9007199254740993), source["extra_body"].(map[string]any)["limit"])
}

func TestClonePreservesNonFiniteConfigurationScalars(t *testing.T) {
	t.Parallel()
	source := map[string]any{"inf": math.Inf(1), "nan": math.Float64frombits(0x7ff8000000000042)}
	copied, _, err := mergeCloneOptions(base.Config{ModelConfig: latest.ModelConfig{ProviderOpts: source}, ModelOptions: options.Apply(options.WithStructuredOutput(&latest.StructuredOutput{Schema: source}))}, nil)
	require.NoError(t, err)
	for key, value := range source {
		assert.Equal(t, math.Float64bits(value.(float64)), math.Float64bits(copied.ProviderOpts[key].(float64)))
	}
}
