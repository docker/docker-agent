package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/internal/deepcopy"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/tools"
)

// ErrNoRebuilder means a provider has no construction function for cloning.
var ErrNoRebuilder = errors.New("provider cannot be cloned without its rebuild function")

// ErrUncopyableConfig means mutable configuration cannot be safely copied.
var ErrUncopyableConfig = errors.New("provider clone requires copyable configuration")

// CloneError identifies the provider whose reconstruction failed.
type CloneError struct {
	ID  modelsdev.ID
	Err error
}

func (e *CloneError) Error() string { return fmt.Sprintf("cloning provider %q: %v", e.ID, e.Err) }
func (e *CloneError) Unwrap() error { return e.Err }

// Clone reconstructs a provider with ordered option overrides, or returns an error.
// Non-zero token caps also constrain routed children, subject to provider floors.
// The last non-zero cap wins; zero retains the latest explicit cap.
func Clone(ctx context.Context, p Provider, opts ...options.Opt) (Provider, error) {
	clone, _, err := cloneProvider(ctx, p, opts)
	return clone, err
}

// CloneWithOptions reconstructs a provider, returning an error-only provider on failure.
//
// Deprecated: use Clone to handle reconstruction errors before making a request.
func CloneWithOptions(ctx context.Context, p Provider, opts ...options.Opt) Provider {
	clone, requested, err := cloneProvider(ctx, p, opts)
	if err == nil {
		return clone
	}
	slog.WarnContext(ctx, "Failed to clone provider", "error", err)
	var id modelsdev.ID
	if p != nil {
		id = p.ID()
	}
	return &failedCloneProvider{config: requested, id: id, err: err}
}

func cloneProvider(ctx context.Context, p Provider, opts []options.Opt) (Provider, base.Config, error) {
	if p == nil {
		return nil, base.Config{}, errors.New("cannot clone a nil provider")
	}
	cfg := p.BaseConfig()
	modelConfig, mergedOpts, err := mergeCloneOptions(cfg, opts)
	if err != nil {
		return nil, base.Config{}, &CloneError{ID: p.ID(), Err: err}
	}
	requested := base.Config{ModelConfig: modelConfig, ModelOptions: options.Apply(mergedOpts...), Env: cfg.Env, BaseURL: cfg.BaseURL}
	if cfg.RebuildProvider == nil {
		return nil, requested, &CloneError{ID: p.ID(), Err: ErrNoRebuilder}
	}
	clone, err := cfg.RebuildProvider(ctx, &modelConfig, mergedOpts...)
	if err == nil && clone == nil {
		err = errors.New("rebuild returned a nil provider")
	}
	if err != nil {
		return nil, requested, &CloneError{ID: p.ID(), Err: err}
	}
	return clone, requested, nil
}

type failedCloneProvider struct {
	config base.Config
	id     modelsdev.ID
	err    error
}

func (p *failedCloneProvider) ID() modelsdev.ID        { return p.id }
func (p *failedCloneProvider) BaseConfig() base.Config { return p.config }
func (p *failedCloneProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return nil, p.err
}

// mergeCloneOptions snapshots mutable state before reconstructing a provider.
func mergeCloneOptions(cfg base.Config, opts []options.Opt) (latest.ModelConfig, []options.Opt, error) {
	// Preserve existing options, then apply overrides. Later opts take precedence.
	baseOptions, err := cfg.ModelOptions.Clone()
	if err != nil {
		return latest.ModelConfig{}, nil, fmt.Errorf("%w: %w", ErrUncopyableConfig, err)
	}
	baseOpts := options.FromModelOptions(baseOptions)
	mergedOpts := slices.Concat(baseOpts, opts)
	var requested options.ModelOptions
	var explicitCap int64
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		opt(&requested)
		if mt := requested.MaxTokens(); mt != 0 {
			explicitCap = mt
		}
	}
	if explicitCap != 0 {
		mergedOpts = append(mergedOpts, options.WithAuxiliaryMaxTokens(explicitCap))
	} else if baseOptions.MaxTokensInheritable() && baseOptions.MaxTokens() != 0 {
		mergedOpts = append(mergedOpts, options.WithAuxiliaryMaxTokens(baseOptions.MaxTokens()))
	}

	// Snapshot overrides too: option setters can carry caller-owned maps.
	merged := options.Apply(mergedOpts...)
	merged, err = merged.Clone()
	if err != nil {
		return latest.ModelConfig{}, nil, fmt.Errorf("%w: %w", ErrUncopyableConfig, err)
	}
	mergedOpts = options.FromModelOptions(merged)

	source := cfg.CloneSource()
	modelConfig, err := copyModelConfig(source)
	if err != nil {
		return latest.ModelConfig{}, nil, err
	}

	if mt := merged.MaxTokens(); mt != 0 {
		modelConfig.MaxTokens = &mt
	}
	if merged.NoThinking() {
		// Write the disabled sentinel instead of nil so the downstream
		// applyProviderDefaults pass cannot revive a provider-level
		// thinking_budget via its setIfNil merge.
		modelConfig.ThinkingBudget = &latest.ThinkingBudget{Effort: "none"}
	}
	return modelConfig, mergedOpts, nil
}

func copyModelConfig(source latest.ModelConfig) (latest.ModelConfig, error) {
	copied, err := deepcopy.Value(source)
	if err != nil {
		return latest.ModelConfig{}, fmt.Errorf("%w: copying model configuration: %w", ErrUncopyableConfig, err)
	}
	return copied, nil
}
