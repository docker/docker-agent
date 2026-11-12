package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/contracts"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/model/provider/rulebased"
	"github.com/docker/docker-agent/pkg/modelinfo"
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
		models, err := copyRoutedModels(cfg, models)
		if err != nil {
			return nil, err
		}
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
		r.attachRebuilder(p, models, env)
		return p, nil
	}
	return r.createDirectProvider(ctx, cfg, env, opts...)
}

// Snapshot only referenced definitions; the rebuilder must not capture caller-owned state.
func copyRoutedModels(cfg *latest.ModelConfig, models map[string]latest.ModelConfig) (map[string]latest.ModelConfig, error) {
	names := []string{cfg.Provider + "/" + cfg.Model}
	for _, rule := range cfg.Routing {
		names = append(names, rule.Model)
	}
	copied := make(map[string]latest.ModelConfig)
	for _, name := range names {
		if _, exists := copied[name]; exists {
			continue
		}
		if model, exists := models[name]; exists {
			clone, err := copyModelConfig(model)
			if err != nil {
				return nil, fmt.Errorf("copying routed model %q: %w", name, err)
			}
			copied[name] = clone
		}
	}
	return copied, nil
}

func (r *Registry) createRuleBasedRouter(ctx context.Context, cfg *latest.ModelConfig, models map[string]latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
	return rulebased.NewClient(ctx, cfg, models, env, r.resolveRoutedModel, opts...)
}

func (r *Registry) resolveRoutedModel(ctx context.Context, modelSpec string, models map[string]latest.ModelConfig, env environment.Provider, factoryOpts ...options.Opt) (rulebased.Provider, error) {
	if modelCfg, exists := models[modelSpec]; exists {
		if len(modelCfg.Routing) > 0 {
			return nil, fmt.Errorf("model %q has routing rules and cannot be used as a routing target", modelSpec)
		}
		copied, err := copyModelConfig(modelCfg)
		if err != nil {
			return nil, fmt.Errorf("copying routed model %q: %w", modelSpec, err)
		}
		return r.createDirectProvider(ctx, &copied, env, factoryOpts...)
	}
	inlineCfg, parseErr := latest.ParseModelRef(modelSpec)
	if parseErr != nil {
		return nil, fmt.Errorf("invalid model spec %q: expected 'provider/model' format or a model reference", modelSpec)
	}
	return r.createDirectProvider(ctx, &inlineCfg, env, factoryOpts...)
}

func (r *Registry) createDirectProvider(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
	if r == nil {
		return nil, errors.New("provider registry is required")
	}
	globalOptions := options.Apply(opts...)
	// Capture explicit off before defaults normalize it to nil. Model settings win.
	budget := cfg.ThinkingBudget
	if budget == nil {
		budget = globalOptions.Providers()[cfg.Provider].ThinkingBudget
	}
	noThinking := globalOptions.NoThinking() || budget.IsDisabled()
	constructionCfg := cfg
	// Config-driven leaves need the same override as option-driven leaves.
	if globalOptions.NoThinking() || globalOptions.MaxTokensInheritable() {
		copied, err := copyModelConfig(*cfg)
		if err != nil {
			return nil, err
		}
		if globalOptions.NoThinking() {
			copied.ThinkingBudget = &latest.ThinkingBudget{Effort: "none"}
		}
		if mt := globalOptions.MaxTokens(); globalOptions.MaxTokensInheritable() && mt != 0 {
			copied.MaxTokens = &mt
		}
		constructionCfg = &copied
	}
	if noThinking && !globalOptions.NoThinking() {
		opts = append(opts, options.WithNoThinking())
	}
	enhancedCfg := applyProviderDefaults(constructionCfg, globalOptions.Providers())
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
	if budget.IsDisabled() && !globalOptions.NoThinking() {
		warnThinkingCannotBeDisabled(ctx, providerType, enhancedCfg)
	}
	factory, ok := r.factory(providerType)
	if !ok {
		slog.ErrorContext(ctx, "Unknown provider type", "type", providerType)
		return nil, unknownProviderError(providerType)
	}
	p, err := factory(ctx, enhancedCfg, env, opts...)
	if err != nil {
		return nil, err
	}
	if setter, ok := p.(interface{ SetDispatchConfig(cfg *latest.ModelConfig) }); ok {
		source, err := copyModelConfig(*constructionCfg)
		if err != nil {
			return nil, err
		}
		setter.SetDispatchConfig(&source)
	}
	r.attachRebuilder(p, nil, env)
	// Wrap leaf providers with the GenAI semconv tracer so every chat
	// completion emits a `chat {model}` CLIENT span and the standard
	// gen_ai.client.* metrics. The rule-based router constructed by
	// createRuleBasedRouter is left bare — its routed targets go through
	// resolveRoutedModel → createDirectProvider and end up wrapped here.
	return instrumentProvider(p), nil
}

// warnThinkingCannotBeDisabled flags a configured thinking_budget none or 0
// that a native Claude transport cannot honor. Internal no-thinking calls
// (titles, sampling) are not configured by the user and stay silent.
func warnThinkingCannotBeDisabled(ctx context.Context, providerType string, cfg *latest.ModelConfig) {
	switch providerType {
	case "anthropic", "amazon-bedrock":
	case "google":
		// Only Model Garden's Anthropic publisher reaches a Claude client. Not
		// vertexai.IsModelGardenConfig: importing vertexai links the OpenAI SDK.
		if publisher, _ := cfg.ProviderOpts["publisher"].(string); !strings.EqualFold(publisher, "anthropic") {
			return
		}
	default:
		return
	}
	if c5, ok := modelinfo.LookupClaude5(cfg.Model); ok && !c5.CanDisableThinking {
		slog.WarnContext(ctx, "model cannot disable thinking; thinking_budget none or 0 is sent as adaptive thinking with low effort",
			"model", cfg.Model, "provider", providerType)
	}
}

func (r *Registry) attachRebuilder(p Provider, models map[string]latest.ModelConfig, env environment.Provider) {
	setter, ok := p.(interface {
		SetProviderRebuilder(rebuild contracts.RebuildProviderFunc)
	})
	if !ok {
		return
	}
	setter.SetProviderRebuilder(func(ctx context.Context, cfg *latest.ModelConfig, opts ...options.Opt) (contracts.Provider, error) {
		return r.NewWithModels(ctx, cfg, models, env, opts...)
	})
}

// EmptyRegistry returns a registry with no provider factories. It is useful
// for components that support running without models; it cannot construct a
// concrete provider until factories are explicitly supplied to NewRegistry.
func EmptyRegistry() *Registry {
	return NewRegistry(nil)
}

// DefaultRegistry returns an empty registry because this package deliberately
// does not import concrete provider implementations.
//
// Deprecated: use EmptyRegistry or construct a Registry explicitly.
func DefaultRegistry() *Registry {
	return EmptyRegistry()
}

func unknownProviderError(providerType string) error {
	return fmt.Errorf("unknown provider type %q (register it with provider.NewRegistry or use providers.NewDefaultRegistry)", providerType)
}
