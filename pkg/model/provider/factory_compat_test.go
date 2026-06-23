package provider

import (
	"context"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/model/provider/rulebased"
	"github.com/docker/docker-agent/pkg/model/provider/vertexai"
)

type providerFactory = Factory

var providerFactories = DefaultRegistry().factories

func testRegistry() *Registry { return NewRegistry(providerFactories) }

func createDirectProvider(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
	return testRegistry().createDirectProvider(ctx, cfg, env, opts...)
}

func resolveRoutedModel(ctx context.Context, modelSpec string, models map[string]latest.ModelConfig, env environment.Provider, factoryOpts ...options.Opt) (rulebased.Provider, error) {
	return testRegistry().resolveRoutedModel(ctx, modelSpec, models, env, factoryOpts...)
}

var (
	geminiClientFactory providerFactory
	vertexClientFactory providerFactory
)

func googleFactory(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider) (Provider, error) {
	if vertexai.IsModelGardenConfig(cfg) {
		return vertexClientFactory(ctx, cfg, env)
	}
	return geminiClientFactory(ctx, cfg, env)
}
