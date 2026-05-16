//go:build !js

package provider

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/anthropic"
	"github.com/docker/docker-agent/pkg/model/provider/bedrock"
	"github.com/docker/docker-agent/pkg/model/provider/dmr"
	"github.com/docker/docker-agent/pkg/model/provider/gemini"
	"github.com/docker/docker-agent/pkg/model/provider/openai"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/model/provider/rulebased"
	"github.com/docker/docker-agent/pkg/model/provider/vertexai"
)

// createRuleBasedRouter creates a rule-based routing provider.
func createRuleBasedRouter(ctx context.Context, cfg *latest.ModelConfig, models map[string]latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
	return rulebased.NewClient(ctx, cfg, models, env, resolveRoutedModel, opts...)
}

// resolveRoutedModel is the rulebased.ProviderFactory used by
// createRuleBasedRouter. It resolves a routing target — which is either a name
// from the models map or an inline "provider/model" spec — and returns the
// provider for it. Routing targets cannot themselves have routing rules.
//
// Defined as a package-level function (rather than an inline closure) so the
// recursion-prevention, parse-error and factory-error paths can be unit-tested
// directly without going through rulebased.NewClient.
func resolveRoutedModel(
	ctx context.Context,
	modelSpec string,
	models map[string]latest.ModelConfig,
	env environment.Provider,
	factoryOpts ...options.Opt,
) (rulebased.Provider, error) {
	// Check if modelSpec is a reference to a model in the models map.
	if modelCfg, exists := models[modelSpec]; exists {
		// Prevent infinite recursion - referenced models cannot have routing rules.
		if len(modelCfg.Routing) > 0 {
			return nil, fmt.Errorf("model %q has routing rules and cannot be used as a routing target", modelSpec)
		}
		return createDirectProvider(ctx, &modelCfg, env, factoryOpts...)
	}

	// Otherwise, treat as an inline model spec (e.g., "openai/gpt-4o").
	inlineCfg, parseErr := latest.ParseModelRef(modelSpec)
	if parseErr != nil {
		return nil, fmt.Errorf("invalid model spec %q: expected 'provider/model' format or a model reference", modelSpec)
	}
	return createDirectProvider(ctx, &inlineCfg, env, factoryOpts...)
}

// createDirectProvider creates a provider without routing (direct model access).
func createDirectProvider(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
	var globalOptions options.ModelOptions
	for _, opt := range opts {
		opt(&globalOptions)
	}

	// Apply defaults from custom providers (from config) or built-in aliases
	enhancedCfg := applyProviderDefaults(cfg, globalOptions.Providers())

	providerType := resolveProviderType(enhancedCfg)

	factory, ok := providerFactories[providerType]
	if !ok {
		slog.ErrorContext(ctx, "Unknown provider type", "type", providerType)
		return nil, fmt.Errorf("unknown provider type: %s", providerType)
	}
	return factory(ctx, enhancedCfg, env, opts...)
}

// providerFactory builds a Provider from a fully-resolved ModelConfig.
// Tests may swap entries in providerFactories to exercise dispatch logic
// without spinning up real provider clients.
type providerFactory func(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error)

// providerFactories maps a resolved provider type (the value returned by
// resolveProviderType) to its constructor. The map is package-private but
// modifiable; tests must restore the original entries with t.Cleanup.
var providerFactories = map[string]providerFactory{
	"openai":                 openaiFactory,
	"openai_chatcompletions": openaiFactory,
	"openai_responses":       openaiFactory,
	"anthropic":              anthropicFactory,
	"google":                 googleFactory,
	"dmr":                    dmrFactory,
	"amazon-bedrock":         bedrockFactory,
}

func openaiFactory(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
	return openai.NewClient(ctx, cfg, env, opts...)
}

func anthropicFactory(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
	return anthropic.NewClient(ctx, cfg, env, opts...)
}

func googleFactory(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
	// Route non-Gemini models on Vertex AI (Model Garden) through the
	// vertexai package, which picks the right endpoint per publisher.
	if vertexai.IsModelGardenConfig(cfg) {
		return vertexClientFactory(ctx, cfg, env, opts...)
	}
	return geminiClientFactory(ctx, cfg, env, opts...)
}

// geminiClientFactory and vertexClientFactory are the inner constructors used
// by googleFactory. They are package-level variables (rather than direct
// references to gemini.NewClient / vertexai.NewClient) so that tests can swap
// them with fakes via t.Cleanup and assert that googleFactory routes correctly
// based on vertexai.IsModelGardenConfig — without spinning up real clients.
var (
	geminiClientFactory providerFactory = func(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
		return gemini.NewClient(ctx, cfg, env, opts...)
	}
	vertexClientFactory providerFactory = func(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
		return vertexai.NewClient(ctx, cfg, env, opts...)
	}
)

func dmrFactory(ctx context.Context, cfg *latest.ModelConfig, _ environment.Provider, opts ...options.Opt) (Provider, error) {
	return dmr.NewClient(ctx, cfg, opts...)
}

func bedrockFactory(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
	return bedrock.NewClient(ctx, cfg, env, opts...)
}
