package provider

import (
	"context"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/anthropic"
	"github.com/docker/docker-agent/pkg/model/provider/bedrock"
	"github.com/docker/docker-agent/pkg/model/provider/gemini"
	"github.com/docker/docker-agent/pkg/model/provider/openai"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/model/provider/vertexai"
)

func fullTestRegistry() *Registry {
	return NewRegistry(map[string]Factory{
		"openai":                 openAITestFactory,
		"openai_chatcompletions": openAITestFactory,
		"openai_responses":       openAITestFactory,
		"anthropic":              anthropicTestFactory,
		"google":                 googleTestFactory,
		"amazon-bedrock":         bedrockTestFactory,
	})
}

func openAITestFactory(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
	return openai.NewClient(ctx, cfg, env, opts...)
}

func anthropicTestFactory(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
	return anthropic.NewClient(ctx, cfg, env, opts...)
}

func googleTestFactory(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
	if vertexai.IsModelGardenConfig(cfg) {
		return vertexai.NewClient(ctx, cfg, env, opts...)
	}
	return gemini.NewClient(ctx, cfg, env, opts...)
}

func bedrockTestFactory(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
	return bedrock.NewClient(ctx, cfg, env, opts...)
}
