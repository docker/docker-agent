// Package rulebased provides a rule-based model router that selects
// the appropriate model based on text similarity using a lightweight
// in-memory BM25 ranker.
//
// A model becomes a rule-based router when it has routing rules configured.
// The model's provider/model fields define the fallback model, and each
// routing rule maps example phrases to different target models.
package rulebased

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/contracts"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/tools"
)

// Provider is the canonical model provider contract.
type Provider = contracts.Provider

// ProviderFactory creates a provider from a model config.
// The models parameter provides access to all configured models for resolving references.
type ProviderFactory func(ctx context.Context, modelSpec string, models map[string]latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error)

// Client implements the Provider interface for rule-based model routing.
type Client struct {
	base.Config

	routes   []Provider
	fallback Provider
	matcher  *matcher
}

// NewClient creates a new rule-based routing client.
// The cfg parameter should have Routing rules configured. The provider/model
// fields of cfg define the fallback model that is used when no routing rule matches.
func NewClient(ctx context.Context, cfg *latest.ModelConfig, models map[string]latest.ModelConfig, env environment.Provider, providerFactory ProviderFactory, opts ...options.Opt) (*Client, error) {
	slog.DebugContext(ctx, "Creating rule-based router", "provider", cfg.Provider, "model", cfg.Model)

	if len(cfg.Routing) == 0 {
		return nil, errors.New("no routing rules configured")
	}

	effective := options.Apply(opts...)
	routeOpts := filterOutMaxTokens(opts)
	// Ordinary router limits stay local; only the effective auxiliary cap propagates.
	if effective.MaxTokensInheritable() && effective.MaxTokens() != 0 {
		routeOpts = append(routeOpts, options.WithAuxiliaryMaxTokens(effective.MaxTokens()))
	}

	// Create fallback provider from the model's provider/model fields.
	fallbackSpec := cfg.Provider + "/" + cfg.Model
	fallback, err := providerFactory(ctx, fallbackSpec, models, env, routeOpts...)
	if err != nil {
		return nil, fmt.Errorf("creating fallback provider %q: %w", fallbackSpec, err)
	}

	client := &Client{
		Config: base.Config{
			ModelConfig:  *cfg,
			ModelOptions: options.Apply(opts...),
			Env:          env,
		},
		matcher:  newMatcher(),
		fallback: fallback,
	}

	// Process routing rules. Each example is indexed under its route index so
	// a match can be mapped back to the corresponding provider.
	for i, rule := range cfg.Routing {
		if rule.Model == "" {
			return nil, fmt.Errorf("routing rule %d: 'model' field is required", i)
		}

		provider, err := providerFactory(ctx, rule.Model, models, env, routeOpts...)
		if err != nil {
			return nil, fmt.Errorf("creating provider for routing rule %q: %w", rule.Model, err)
		}

		routeIndex := len(client.routes)
		client.routes = append(client.routes, provider)

		for _, example := range rule.Examples {
			client.matcher.add(routeIndex, example)
		}
	}

	return client, nil
}

// filterOutMaxTokens removes WithMaxTokens options from the slice.
// Child providers may have different token limits than the parent router.
func filterOutMaxTokens(opts []options.Opt) []options.Opt {
	var filtered []options.Opt
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		var probe options.ModelOptions
		opt(&probe)
		if probe.MaxTokens() != 0 {
			continue
		}
		filtered = append(filtered, opt)
	}
	return filtered
}

// CreateChatCompletionStream selects a provider based on input and delegates
// the call. The returned stream reports the selected provider as its
// ServingModel.
func (c *Client) CreateChatCompletionStream(
	ctx context.Context,
	messages []chat.Message,
	availableTools []tools.Tool,
) (chat.MessageStream, error) {
	provider, selfServe := c.selectProvider(messages)
	if provider == nil {
		return nil, errors.New("no provider available for routing")
	}

	selectedID := provider.ID()
	slog.DebugContext(ctx, "Rule-based router selected model",
		"router", c.ID().String(),
		"selected_model", selectedID.String(),
		"message_count", len(messages),
	)

	stream, err := provider.CreateChatCompletionStream(ctx, messages, availableTools)
	if err != nil || stream == nil {
		return stream, err
	}
	serving := contracts.ServingModel{ID: selectedID, Config: provider.BaseConfig()}
	if selfServe {
		serving.ID = c.ID()
		// The default provider is built from provider/model alone; preserve
		// the router's accounting overrides without losing the resolved endpoint.
		if serving.Config.ModelConfig.Cost == nil {
			serving.Config.ModelConfig.Cost = c.ModelConfig.Cost
		}
		if serving.Config.ModelConfig.BaseURL == "" {
			serving.Config.ModelConfig.BaseURL = c.ModelConfig.BaseURL
		}
	}
	if inner, ok := stream.(contracts.ServingStream); ok {
		serving = inner.ServingModel()
	}
	return &routedStream{MessageStream: stream, serving: serving}, nil
}

// selectProvider finds the best matching provider for the messages.
func (c *Client) selectProvider(messages []chat.Message) (Provider, bool) {
	userMessage := getLastUserMessage(messages)
	if userMessage == "" {
		return c.defaultProvider(), true
	}

	routeIdx, ok := c.matcher.bestRoute(userMessage)
	if !ok || routeIdx >= len(c.routes) {
		return c.defaultProvider(), true
	}

	selected := c.routes[routeIdx]
	slog.Debug("Route matched", "model", selected.ID().String())
	return selected, false
}

func (c *Client) defaultProvider() Provider {
	if c.fallback != nil {
		return c.fallback
	}
	if len(c.routes) > 0 {
		return c.routes[0]
	}
	return nil
}

func getLastUserMessage(messages []chat.Message) string {
	for _, message := range slices.Backward(messages) {
		if message.Role == chat.MessageRoleUser {
			return message.Content
		}
	}
	return ""
}

// BaseConfig returns the base configuration.
func (c *Client) BaseConfig() base.Config {
	return c.Config
}

// Close cleans up resources.
func (c *Client) Close() error {
	return nil
}

type routedStream struct {
	chat.MessageStream

	serving contracts.ServingModel
}

func (s *routedStream) ServingModel() contracts.ServingModel {
	return s.serving
}

func (s *routedStream) ServedBy() (contracts.ServingModel, bool) {
	if served, ok := s.MessageStream.(contracts.ServedStream); ok {
		return served.ServedBy()
	}
	return contracts.ServingModel{}, false
}
