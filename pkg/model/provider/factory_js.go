//go:build js && wasm

// Package provider's js/wasm factory.
//
// The non-js factory.go pulls in every provider — including dmr (os/exec),
// rulebased (bleve, mmap), bedrock and vertexai (cloud SDKs). None of those
// can be cross-compiled to js/wasm, so this file replaces factory.go under
// js/wasm with a slim variant that only knows about the providers that work
// over plain net/http (which the Go runtime maps to fetch in the browser):
//
//   - openai / openai_chatcompletions / openai_responses
//   - anthropic
//   - google (Gemini API; Vertex AI is unsupported under wasm)
//
// Routing rules and Docker Model Runner are unsupported and return an error.
package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/anthropic"
	"github.com/docker/docker-agent/pkg/model/provider/gemini"
	"github.com/docker/docker-agent/pkg/model/provider/openai"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

// errRoutingUnsupported is returned by createRuleBasedRouter under js/wasm:
// the rulebased provider depends on bleve which cannot be cross-compiled to
// wasm (mmap, file locking).
var errRoutingUnsupported = errors.New("rule-based model routing is not supported under js/wasm")

// createRuleBasedRouter is a stub that always returns an error under wasm.
func createRuleBasedRouter(_ context.Context, _ *latest.ModelConfig, _ map[string]latest.ModelConfig, _ environment.Provider, _ ...options.Opt) (Provider, error) {
	return nil, errRoutingUnsupported
}

// createDirectProvider mirrors the non-wasm version but only dispatches to
// providers that are reachable from a browser.
func createDirectProvider(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
	var globalOptions options.ModelOptions
	for _, opt := range opts {
		opt(&globalOptions)
	}

	enhancedCfg := applyProviderDefaults(cfg, globalOptions.Providers())

	providerType := resolveProviderType(enhancedCfg)

	factory, ok := providerFactories[providerType]
	if !ok {
		slog.ErrorContext(ctx, "Unknown or unsupported provider type under js/wasm", "type", providerType)
		return nil, fmt.Errorf("provider type %q is not supported under js/wasm (only openai/anthropic/google work in the browser)", providerType)
	}
	return factory(ctx, enhancedCfg, env, opts...)
}

type providerFactory func(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error)

// providerFactories: js/wasm-only subset. dmr (os/exec), amazon-bedrock and
// vertex AI (cloud SDKs that don't compile to wasm) are deliberately absent.
var providerFactories = map[string]providerFactory{
	"openai":                 openaiFactory,
	"openai_chatcompletions": openaiFactory,
	"openai_responses":       openaiFactory,
	"anthropic":              anthropicFactory,
	"google":                 googleFactory,
}

func openaiFactory(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
	return openai.NewClient(ctx, cfg, env, opts...)
}

func anthropicFactory(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
	return anthropic.NewClient(ctx, cfg, env, opts...)
}

func googleFactory(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
	return gemini.NewClient(ctx, cfg, env, opts...)
}
