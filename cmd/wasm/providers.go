//go:build js && wasm

package main

import (
	"context"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/openai"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

// The demo registry: OpenAI-compatible providers in every build, plus the
// providers of the build's profile (small.go or small_off.go). Embedders
// supply their own registry without importing these SDKs through the core
// provider package.
var demoProviders = provider.NewRegistry(demoFactories())

func demoFactories() map[string]provider.Factory {
	factories := map[string]provider.Factory{
		"openai":                 openaiFactory,
		"openai_chatcompletions": openaiFactory,
		"openai_responses":       openaiFactory,
	}
	registerProviders(factories)
	return factories
}

func openaiFactory(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (provider.Provider, error) {
	return openai.NewClient(ctx, cfg, env, opts...)
}
