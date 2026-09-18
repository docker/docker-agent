// Package provider defines the provider contracts and builds providers from
// explicit factory registries.
//
// The package deliberately does not import concrete SDK-backed providers.
// Applications that need Docker Agent's built-in provider set should import
// pkg/model/provider/providers and use its NewDefaultRegistry. Embedders can
// instead build a smaller registry with [NewRegistry]. [EmptyRegistry] is
// available for components that support running without model providers.
package provider

import (
	"context"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/contracts"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

// Provider is the common interface implemented by model providers.
type Provider = contracts.Provider

// EmbeddingProvider is a provider that supports embeddings.
type EmbeddingProvider = contracts.EmbeddingProvider

// BatchEmbeddingProvider is an embedding provider that supports batches.
type BatchEmbeddingProvider = contracts.BatchEmbeddingProvider

// RerankingProvider is a provider that can score documents by relevance.
type RerankingProvider = contracts.RerankingProvider

// New creates a provider with an empty registry and therefore returns an
// unknown-provider error for every concrete provider.
//
// Deprecated: construct a Registry explicitly and call Registry.New.
func New(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
	return EmptyRegistry().New(ctx, cfg, env, opts...)
}

// NewWithModels creates a provider with an empty registry and therefore
// returns an unknown-provider error for every concrete provider.
//
// Deprecated: construct a Registry explicitly and call Registry.NewWithModels.
func NewWithModels(ctx context.Context, cfg *latest.ModelConfig, models map[string]latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
	return EmptyRegistry().NewWithModels(ctx, cfg, models, env, opts...)
}

// ContextWindowResolver returns an optional live context-window lookup, preserving
// access through instrumentation wrappers. Providers without discovery return nil.
func ContextWindowResolver(p Provider) func(context.Context) (int64, error) {
	if resolver, ok := unwrapProvider(p).(interface {
		ContextWindow(ctx context.Context) (int64, error)
	}); ok {
		return resolver.ContextWindow
	}
	return nil
}
