// Package providers wires every built-in model provider into a
// provider.Registry. Importing it links all provider SDKs; embedders that
// want a smaller binary build their own registry from the providers they
// use, e.g. provider.NewRegistry(map[string]provider.Factory{"anthropic":
// provider.Adapt(anthropic.NewClient)}).
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

// DefaultFactories maps every built-in provider type to its factory.
// Callers may copy and trim it before building a registry.
func DefaultFactories() map[string]provider.Factory {
	openaiFactory := provider.Adapt(openai.NewClient)
	return map[string]provider.Factory{
		"openai":                 openaiFactory,
		"openai_chatcompletions": openaiFactory,
		"openai_responses":       openaiFactory,
		"anthropic":              provider.Adapt(anthropic.NewClient),
		"google":                 Google,
		"dmr":                    DMR,
		"amazon-bedrock":         provider.Adapt(bedrock.NewClient),
	}
}

// Google serves the "google" provider type: Gemini models through the Gemini
// API, or Vertex AI Model Garden when the config asks for it.
func Google(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (provider.Provider, error) {
	if vertexai.IsModelGardenConfig(cfg) {
		return vertexai.NewClient(ctx, cfg, env, opts...)
	}
	return gemini.NewClient(ctx, cfg, env, opts...)
}

// DMR serves the Docker Model Runner provider type.
func DMR(ctx context.Context, cfg *latest.ModelConfig, _ environment.Provider, opts ...options.Opt) (provider.Provider, error) {
	return dmr.NewClient(ctx, cfg, opts...)
}
