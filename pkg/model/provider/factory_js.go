//go:build js && wasm

package provider

import (
	"context"
	"fmt"
	"log/slog"
	"maps"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/model/provider/rulebased"
)

type Factory func(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error)

type Registry struct {
	factories map[string]Factory
}

func NewRegistry(factories map[string]Factory) *Registry {
	copied := make(map[string]Factory, len(factories))
	maps.Copy(copied, factories)
	return &Registry{factories: copied}
}

var defaultFactories map[string]Factory

func DefaultRegistry() *Registry { return NewRegistry(defaultFactories) }

func (r *Registry) New(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
	return r.NewWithModels(ctx, cfg, nil, env, opts...)
}

func (r *Registry) NewWithModels(ctx context.Context, cfg *latest.ModelConfig, models map[string]latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
	slog.DebugContext(ctx, "Creating model provider", "type", cfg.Provider, "model", cfg.Model)
	if len(cfg.Routing) > 0 {
		p, err := r.createRuleBasedRouter(ctx, cfg, models, env, opts...)
		if err != nil {
			return nil, err
		}
		if setter, ok := p.(interface{ SetProviderRegistry(registry any) }); ok {
			setter.SetProviderRegistry(r)
		}
		return p, nil
	}
	return r.createDirectProvider(ctx, cfg, env, opts...)
}

func (r *Registry) createRuleBasedRouter(ctx context.Context, cfg *latest.ModelConfig, models map[string]latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
	return rulebased.NewClient(ctx, cfg, models, env, r.resolveRoutedModel, opts...)
}

func (r *Registry) resolveRoutedModel(ctx context.Context, modelSpec string, models map[string]latest.ModelConfig, env environment.Provider, factoryOpts ...options.Opt) (rulebased.Provider, error) {
	if modelCfg, exists := models[modelSpec]; exists {
		if len(modelCfg.Routing) > 0 {
			return nil, fmt.Errorf("model %q has routing rules and cannot be used as a routing target", modelSpec)
		}
		return r.createDirectProvider(ctx, &modelCfg, env, factoryOpts...)
	}
	inlineCfg, parseErr := latest.ParseModelRef(modelSpec)
	if parseErr != nil {
		return nil, fmt.Errorf("invalid model spec %q: expected 'provider/model' format or a model reference", modelSpec)
	}
	return r.createDirectProvider(ctx, &inlineCfg, env, factoryOpts...)
}

func (r *Registry) createDirectProvider(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
	if r == nil {
		r = DefaultRegistry()
	}
	var globalOptions options.ModelOptions
	for _, opt := range opts {
		opt(&globalOptions)
	}
	enhancedCfg := applyProviderDefaults(cfg, globalOptions.Providers())
	providerType := resolveProviderType(enhancedCfg)
	factory, ok := r.factories[providerType]
	if !ok {
		slog.ErrorContext(ctx, "Unknown or unsupported provider type under js/wasm", "type", providerType)
		return nil, fmt.Errorf("provider type %q is not registered", providerType)
	}
	p, err := factory(ctx, enhancedCfg, env, opts...)
	if err != nil {
		return nil, err
	}
	if setter, ok := p.(interface{ SetProviderRegistry(registry any) }); ok {
		setter.SetProviderRegistry(r)
	}
	return p, nil
}
