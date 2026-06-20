package providers

import (
	"context"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/anthropic"
	"github.com/docker/docker-agent/pkg/model/provider/bedrock"
	"github.com/docker/docker-agent/pkg/model/provider/dmr"
	"github.com/docker/docker-agent/pkg/model/provider/gemini"
	"github.com/docker/docker-agent/pkg/model/provider/openai"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/model/provider/vertexai"
)

func NewDefaultRegistry() *provider.Registry {
	return provider.NewRegistry(DefaultFactories())
}

func DefaultFactories() map[string]provider.Factory {
	return map[string]provider.Factory{
		"openai":                 openaiFactory,
		"openai_chatcompletions": openaiFactory,
		"openai_responses":       openaiFactory,
		"anthropic":              anthropicFactory,
		"google":                 googleFactory,
		"dmr":                    dmrFactory,
		"amazon-bedrock":         bedrockFactory,
	}
}

func openaiFactory(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (provider.Provider, error) {
	return openai.NewClient(ctx, cfg, env, opts...)
}

func anthropicFactory(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (provider.Provider, error) {
	return anthropic.NewClient(ctx, cfg, env, opts...)
}

func googleFactory(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (provider.Provider, error) {
	if vertexai.IsModelGardenConfig(cfg) {
		return vertexai.NewClient(ctx, cfg, env, opts...)
	}
	return gemini.NewClient(ctx, cfg, env, opts...)
}

func dmrFactory(ctx context.Context, cfg *latest.ModelConfig, _ environment.Provider, opts ...options.Opt) (provider.Provider, error) {
	return dmr.NewClient(ctx, cfg, opts...)
}

func bedrockFactory(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (provider.Provider, error) {
	return bedrock.NewClient(ctx, cfg, env, opts...)
}
