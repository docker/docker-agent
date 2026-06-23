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
	"sync"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/tools"
)

// Provider defines the minimal interface needed for model providers.
type Provider interface {
	ID() modelsdev.ID
	CreateChatCompletionStream(
		ctx context.Context,
		messages []chat.Message,
		availableTools []tools.Tool,
	) (chat.MessageStream, error)
	BaseConfig() base.Config
}

// ProviderFactory creates a provider from a model config.
// The models parameter provides access to all configured models for resolving references.
type ProviderFactory func(ctx context.Context, modelSpec string, models map[string]latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error)

// Client implements the Provider interface for rule-based model routing.
type Client struct {
	base.Config

	routes         []Provider
	fallback       Provider
	matcher        *matcher
	mu             sync.RWMutex
	lastSelectedID modelsdev.ID // ID of the provider selected by the most recent call
}

// NewClient creates a new rule-based routing client.
// The cfg parameter should have Routing rules configured. The provider/model
// fields of cfg define the fallback model that is used when no routing rule matches.
func NewClient(ctx context.Context, cfg *latest.ModelConfig, models map[string]latest.ModelConfig, env environment.Provider, providerFactory ProviderFactory, opts ...options.Opt) (*Client, error) {
	slog.DebugContext(ctx, "Creating rule-based router", "provider", cfg.Provider, "model", cfg.Model)

	if len(cfg.Routing) == 0 {
		return nil, errors.New("no routing rules configured")
	}

	routeOpts := filterOutMaxTokens(opts)

	// Create fallback provider from the model's provider/model fields.
	fallbackSpec := cfg.Provider + "/" + cfg.Model
	fallback, err := providerFactory(ctx, fallbackSpec, models, env, routeOpts...)
	if err != nil {
		return nil, fmt.Errorf("creating fallback provider %q: %w", fallbackSpec, err)
	}

	client := &Client{
		Config: base.Config{
			ModelConfig: *cfg,
			Models:      models,
			Env:         env,
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

// CreateChatCompletionStream selects a provider based on input and delegates the call.
// The selected provider's ID is recorded in LastSelectedModelID.
func (c *Client) CreateChatCompletionStream(
	ctx context.Context,
	messages []chat.Message,
	availableTools []tools.Tool,
) (chat.MessageStream, error) {
	provider := c.selectProvider(messages)
	if provider == nil {
		return nil, errors.New("no provider available for routing")
	}

	selectedID := provider.ID()
	c.mu.Lock()
	c.lastSelectedID = selectedID
	c.mu.Unlock()
	slog.DebugContext(ctx, "Rule-based router selected model",
		"router", c.ID().String(),
		"selected_model", selectedID.String(),
		"message_count", len(messages),
	)

	return provider.CreateChatCompletionStream(ctx, messages, availableTools)
}

// LastSelectedModelID returns the ID of the provider selected by the most
// recent CreateChatCompletionStream call. This allows callers to display
// the YAML-configured sub-model name for rule-based routing.
func (c *Client) LastSelectedModelID() modelsdev.ID {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastSelectedID
}

// selectProvider finds the best matching provider for the messages.
func (c *Client) selectProvider(messages []chat.Message) Provider {
	userMessage := getLastUserMessage(messages)
	if userMessage == "" {
		return c.defaultProvider()
	}

	routeIdx, ok := c.matcher.bestRoute(userMessage)
	if !ok || routeIdx >= len(c.routes) {
		return c.defaultProvider()
	}

	selected := c.routes[routeIdx]
	slog.Debug("Route matched", "model", selected.ID().String())
	return selected
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
