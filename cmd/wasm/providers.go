//go:build js && wasm

package main

import (
	"context"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/anthropic"
	"github.com/docker/docker-agent/pkg/model/provider/gemini"
	"github.com/docker/docker-agent/pkg/model/provider/openai"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

// The demo supports all browser providers. Embedders supply their own registry
// without importing these SDKs through the core provider package.
var demoProviders = provider.NewRegistry(map[string]provider.Factory{
	"anthropic": func(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (provider.Provider, error) {
		return anthropic.NewClient(ctx, cfg, env, opts...)
	},
	"google": func(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (provider.Provider, error) {
		return gemini.NewClient(ctx, cfg, env, opts...)
	},
	"openai":                 openaiFactory,
	"openai_chatcompletions": openaiFactory,
	"openai_responses":       openaiFactory,
})

func openaiFactory(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (provider.Provider, error) {
	return openai.NewClient(ctx, cfg, env, opts...)
}
