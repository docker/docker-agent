//go:build js && wasm

package main

import (
	"context"
	"maps"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/anthropic"
	"github.com/docker/docker-agent/pkg/model/provider/openai"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

// The demo supports all browser providers. Embedders supply their own registry
// without importing these SDKs through the core provider package.
var demoProviders = provider.NewRegistry(demoFactories())

// demoFactories is the default browser set plus the cloud providers of
// cloud.go when the binary is built with -tags docker_agent_wasm_cloud;
// googleFactory comes from cloud.go or cloud_off.go accordingly.
func demoFactories() map[string]provider.Factory {
	factories := map[string]provider.Factory{
		"anthropic": func(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (provider.Provider, error) {
			return anthropic.NewClient(ctx, cfg, env, opts...)
		},
		"google":                 googleFactory,
		"openai":                 openaiFactory,
		"openai_chatcompletions": openaiFactory,
		"openai_responses":       openaiFactory,
	}
	maps.Copy(factories, cloudFactories())
	return factories
}

func openaiFactory(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (provider.Provider, error) {
	return openai.NewClient(ctx, cfg, env, opts...)
}
