package provider

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

// TestResolveRoutedModel_NamedReference verifies that a model name in the
// models map is resolved to its config and dispatched through the registry.
func TestResolveRoutedModel_NamedReference(t *testing.T) {
	var capturedCfg *latest.ModelConfig
	withFactories(t, map[string]providerFactory{
		"openai": func(_ context.Context, cfg *latest.ModelConfig, _ environment.Provider, _ ...options.Opt) (Provider, error) {
			capturedCfg = cfg
			return &fakeProvider{id: modelsdev.NewID("openai", "captured")}, nil
		},
	})

	models := map[string]latest.ModelConfig{
		"fast": {Provider: "openai", Model: "gpt-4o-mini"},
	}

	p, err := resolveRoutedModel(t.Context(), "fast", models, environment.NewNoEnvProvider())
	require.NoError(t, err)
	require.NotNil(t, p)

	require.NotNil(t, capturedCfg)
	assert.Equal(t, "openai", capturedCfg.Provider)
	assert.Equal(t, "gpt-4o-mini", capturedCfg.Model)
}

// TestResolveRoutedModel_InlineSpec verifies that a "provider/model" string
// not present in the models map is parsed as an inline reference.
func TestResolveRoutedModel_InlineSpec(t *testing.T) {
	var capturedCfg *latest.ModelConfig
	withFactories(t, map[string]providerFactory{
		"openai": func(_ context.Context, cfg *latest.ModelConfig, _ environment.Provider, _ ...options.Opt) (Provider, error) {
			capturedCfg = cfg
			return &fakeProvider{id: modelsdev.NewID("openai", "captured")}, nil
		},
	})

	p, err := resolveRoutedModel(t.Context(), "openai/gpt-4o", nil, environment.NewNoEnvProvider())
	require.NoError(t, err)
	require.NotNil(t, p)

	require.NotNil(t, capturedCfg)
	assert.Equal(t, "openai", capturedCfg.Provider)
	assert.Equal(t, "gpt-4o", capturedCfg.Model)
}

// TestResolveRoutedModel_InvalidInlineSpec covers the previously-unreachable
// ParseModelRef error branch (modelSpec not in map and not parseable).
func TestResolveRoutedModel_InvalidInlineSpec(t *testing.T) {
	withFactories(t, map[string]providerFactory{
		"openai": tagFactory("openai"),
	})

	_, err := resolveRoutedModel(t.Context(), "no-slash-here", nil, environment.NewNoEnvProvider())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid model spec")
	assert.Contains(t, err.Error(), "no-slash-here")
}

// TestResolveRoutedModel_RecursiveRoutingRejected covers the recursion-prevention
// branch: a routing target may not itself have routing rules.
func TestResolveRoutedModel_RecursiveRoutingRejected(t *testing.T) {
	withFactories(t, map[string]providerFactory{
		"openai": tagFactory("openai"),
	})

	models := map[string]latest.ModelConfig{
		"router-as-target": {
			Provider: "openai",
			Model:    "gpt-4o",
			Routing:  []latest.RoutingRule{{Model: "x", Examples: []string{"hi"}}},
		},
	}

	_, err := resolveRoutedModel(t.Context(), "router-as-target", models, environment.NewNoEnvProvider())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "routing rules")
	assert.Contains(t, err.Error(), "router-as-target")
}

// TestResolveRoutedModel_FactoryErrorPropagated_NamedRef verifies that when a
// named model's factory fails, the error is returned to the caller.
func TestResolveRoutedModel_FactoryErrorPropagated_NamedRef(t *testing.T) {
	sentinel := errors.New("named-fail")
	withFactories(t, map[string]providerFactory{
		"openai": func(_ context.Context, _ *latest.ModelConfig, _ environment.Provider, _ ...options.Opt) (Provider, error) {
			return nil, sentinel
		},
	})

	models := map[string]latest.ModelConfig{
		"fast": {Provider: "openai", Model: "gpt-4o-mini"},
	}

	_, err := resolveRoutedModel(t.Context(), "fast", models, environment.NewNoEnvProvider())
	require.ErrorIs(t, err, sentinel)
}

// TestResolveRoutedModel_FactoryErrorPropagated_Inline verifies the same for
// an inline "provider/model" spec.
func TestResolveRoutedModel_FactoryErrorPropagated_Inline(t *testing.T) {
	sentinel := errors.New("inline-fail")
	withFactories(t, map[string]providerFactory{
		"openai": func(_ context.Context, _ *latest.ModelConfig, _ environment.Provider, _ ...options.Opt) (Provider, error) {
			return nil, sentinel
		},
	})

	_, err := resolveRoutedModel(t.Context(), "openai/gpt-4o", nil, environment.NewNoEnvProvider())
	require.ErrorIs(t, err, sentinel)
}

// TestResolveRoutedModel_OptionsForwarded verifies that factoryOpts reach the
// downstream factory unchanged. This guards against accidentally dropping
// options (e.g. WithProviders) when extracting the closure.
func TestResolveRoutedModel_OptionsForwarded(t *testing.T) {
	var capturedOpts []options.Opt
	withFactories(t, map[string]providerFactory{
		"openai": func(_ context.Context, _ *latest.ModelConfig, _ environment.Provider, opts ...options.Opt) (Provider, error) {
			capturedOpts = opts
			return &fakeProvider{id: modelsdev.NewID("openai", "ok")}, nil
		},
	})

	maxTokens := int64(2048)
	_, err := resolveRoutedModel(
		t.Context(), "openai/gpt-4o", nil, environment.NewNoEnvProvider(),
		options.WithMaxTokens(maxTokens),
	)
	require.NoError(t, err)

	require.NotEmpty(t, capturedOpts)
	var probe options.ModelOptions
	for _, o := range capturedOpts {
		o(&probe)
	}
	assert.Equal(t, maxTokens, probe.MaxTokens())
}
