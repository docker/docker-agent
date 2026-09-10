package provider

import (
	"context"
	"fmt"
	"log/slog"
	"maps"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/model/provider/rulebased"
)

type Factory func(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error)

type Registry struct {
	factories map[string]Factory
}

func NewRegistry(factories map[string]Factory) *Registry {
	copied := make(map[string]Factory, len(factories))
	maps.Copy(copied, factories)
	return &Registry{factories: copied}
}

func (r *Registry) New(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
	return r.NewWithModels(ctx, cfg, nil, env, opts...)
}

func (r *Registry) NewWithModels(ctx context.Context, cfg *latest.ModelConfig, models map[string]latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
	slog.DebugContext(ctx, "Creating model provider", "type", cfg.Provider, "model", cfg.Model)
	if len(cfg.Routing) > 0 {
		// A router makes no HTTP calls itself; its fallback and routed targets
		// do. When the router model opts out of the gateway, propagate the
		// bypass to every child so the whole routing subtree dials directly.
		// Routed targets that are named models can still set their own flag.
		if cfg.BypassModelsGateway {
			globalOptions := options.Apply(opts...)
			if globalOptions.Gateway() != "" {
				slog.DebugContext(ctx, "Bypassing models gateway for routing model", "provider", cfg.Provider, "model", cfg.Model)
				opts = append(opts, options.WithGateway(""))
			}
		}
		p, err := r.createRuleBasedRouter(ctx, cfg, models, env, opts...)
		if err != nil {
			return nil, err
		}
		if setter, ok := p.(interface{ SetProviderRegistry(registry any) }); ok {
			setter.SetProviderRegistry(r)
		}
		return p, nil
	}
	return r.createDirectProvider(ctx, cfg, env, opts...)
}

func (r *Registry) createRuleBasedRouter(ctx context.Context, cfg *latest.ModelConfig, models map[string]latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
	return rulebased.NewClient(ctx, cfg, models, env, r.resolveRoutedModel, opts...)
}

func (r *Registry) resolveRoutedModel(ctx context.Context, modelSpec string, models map[string]latest.ModelConfig, env environment.Provider, factoryOpts ...options.Opt) (rulebased.Provider, error) {
	if modelCfg, exists := models[modelSpec]; exists {
		if len(modelCfg.Routing) > 0 {
			return nil, fmt.Errorf("model %q has routing rules and cannot be used as a routing target", modelSpec)
		}
		return r.createDirectProvider(ctx, &modelCfg, env, factoryOpts...)
	}
	inlineCfg, parseErr := latest.ParseModelRef(modelSpec)
	if parseErr != nil {
		return nil, fmt.Errorf("invalid model spec %q: expected 'provider/model' format or a model reference", modelSpec)
	}
	return r.createDirectProvider(ctx, &inlineCfg, env, factoryOpts...)
}

func (r *Registry) createDirectProvider(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
	if r == nil {
		r = DefaultRegistry()
	}
	globalOptions := options.Apply(opts...)
	enhancedCfg := applyProviderDefaults(cfg, globalOptions.Providers())
	if err := expandModelConfigEnv(ctx, enhancedCfg, env); err != nil {
		return nil, err
	}
	// Resolve genuine-OpenAI-vendor identity now that custom providers and
	// aliases are fully applied, and thread it to the leaf factory as trusted
	// internal state rather than a ProviderOpts key: provider_opts is public,
	// user-controllable config, so it must never be able to spoof or suppress
	// this decision (see options.WithOpenAIVendor).
	opts = append(opts, options.WithOpenAIVendor(isOpenAIVendor(enhancedCfg)))
	// A model may opt out of the models gateway and dial its provider directly.
	// Clearing the gateway option makes the leaf provider take its direct-auth
	// path (provider API key / token_key) instead of the gateway path.
	// A custom base_url (set on the model or inherited from a custom provider
	// definition) implies the bypass: the endpoint was explicitly chosen by the
	// user and is not one the gateway serves. Checked on the pre-defaults cfg so
	// built-in alias default base URLs don't count.
	bypass := enhancedCfg.BypassModelsGateway || latest.HasCustomBaseURL(*cfg, globalOptions.Providers())
	if bypass && globalOptions.Gateway() != "" {
		slog.DebugContext(ctx, "Bypassing models gateway for model", "provider", enhancedCfg.Provider, "model", enhancedCfg.Model)
		opts = append(opts, options.WithGateway(""))
	}
	providerType := resolveProviderType(enhancedCfg)
	factory, ok := r.factory(providerType)
	if !ok {
		slog.ErrorContext(ctx, "Unknown provider type", "type", providerType)
		return nil, unknownProviderError(providerType)
	}
	p, err := factory(ctx, enhancedCfg, env, opts...)
	if err != nil {
		return nil, err
	}
	if setter, ok := p.(interface{ SetProviderRegistry(registry any) }); ok {
		setter.SetProviderRegistry(r)
	}
	// Wrap leaf providers with the GenAI semconv tracer so every chat
	// completion emits a `chat {model}` CLIENT span and the standard
	// gen_ai.client.* metrics. The rule-based router constructed by
	// createRuleBasedRouter is left bare — its routed targets go through
	// resolveRoutedModel → createDirectProvider and end up wrapped here.
	return instrumentProvider(p), nil
}

var defaultFactories map[string]Factory

func DefaultRegistry() *Registry {
	return NewRegistry(defaultFactories)
}

func unknownProviderError(providerType string) error {
	return fmt.Errorf("unknown provider type %q (register it with provider.NewRegistry or use providers.NewDefaultRegistry)", providerType)
}
