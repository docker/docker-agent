// Package contracts defines the interfaces and shared state used by model providers.
// It has no dependency on provider construction, allowing implementations and
// orchestration packages to share one canonical contract.
package contracts

import (
	"cmp"
	"context"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/catalog"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/model/provider/providerutil"
	"github.com/docker/docker-agent/pkg/modelinfo"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/rag/types"
	"github.com/docker/docker-agent/pkg/tools"
)

// RebuildProviderFunc reconstructs a provider with adjusted options.
type RebuildProviderFunc func(
	ctx context.Context,
	cfg *latest.ModelConfig,
	opts ...options.Opt,
) (Provider, error)

// Config is the common configuration embedded by provider clients.
type Config struct {
	// DispatchConfig keeps construction identity separate from catalogue identity.
	DispatchConfig *latest.ModelConfig
	ModelConfig    latest.ModelConfig
	ModelOptions   options.ModelOptions
	Env            environment.Provider
	// RebuildProvider preserves the construction context needed to clone a provider.
	RebuildProvider RebuildProviderFunc
	// BaseURL is the resolved endpoint actually used by the provider, unlike
	// ModelConfig.BaseURL, which contains the user-supplied value.
	BaseURL string
}

// Endpoint returns where requests go: the user-supplied base_url, else the
// endpoint resolved at construction (e.g. from OPENAI_BASE_URL).
func (c *Config) Endpoint() string {
	return cmp.Or(c.ModelConfig.BaseURL, c.BaseURL)
}

// SetDispatchConfig takes ownership of a caller-isolated configuration for reconstruction.
func (c *Config) SetDispatchConfig(cfg *latest.ModelConfig) { c.DispatchConfig = cfg }

// CloneSource returns the construction configuration, not the catalogue identity.
func (c *Config) CloneSource() latest.ModelConfig {
	if c.DispatchConfig != nil {
		return *c.DispatchConfig
	}
	return c.ModelConfig
}

// SetProviderRebuilder sets the function used to reconstruct the provider.
func (c *Config) SetProviderRebuilder(rebuild RebuildProviderFunc) {
	c.RebuildProvider = rebuild
}

// ID returns the provider-qualified identity, preferring the original display
// model name over a resolved or pinned model name.
func (c *Config) ID() modelsdev.ID {
	return modelsdev.NewID(c.ModelConfig.Provider, c.ModelConfig.DisplayOrModel())
}

func (c *Config) BaseConfig() Config {
	return *c
}

// TrackUsageEnabled reports whether token-usage tracking is enabled.
func (c *Config) TrackUsageEnabled() bool {
	return c.ModelConfig.TrackUsage == nil || *c.ModelConfig.TrackUsage
}

// CapsOverride returns explicit attachment capabilities. These overrides take
// precedence when a custom or aliased provider cannot be resolved in models.dev.
func (c *Config) CapsOverride() *modelinfo.CapsOverride {
	caps := c.ModelConfig.Capabilities
	if caps == nil {
		return nil
	}
	return &modelinfo.CapsOverride{Image: caps.Image, PDF: caps.PDF, Audio: caps.Audio, Video: caps.Video}
}

// ToolCallSupport resolves the model's tool-call capability.
func (c *Config) ToolCallSupport(ctx context.Context) modelinfo.ToolCallSupport {
	return modelinfo.ResolveToolCallSupport(ctx, c.ModelOptions.ModelsDevStore(), c.ID())
}

// ImageOutputEnabled resolves the model's image-output capability.
func (c *Config) ImageOutputEnabled(ctx context.Context) bool {
	var override *bool
	if caps := c.ModelConfig.OutputCapabilities; caps != nil {
		override = caps.Image
	}
	return modelinfo.ResolveOutputImage(ctx, c.ModelOptions.ModelsDevStore(), c.ID(), override)
}

// NativeToolSearchEnabled reports whether provider_opts.native_tool_search
// opts this model into provider-hosted tool search. Strictly opt-in and gated
// to verified endpoints (see [modelinfo.SupportsHostedToolSearch]); only then
// do [tools.Tool.InCatalog] tools reach the provider through tool search. A
// custom [Config.Endpoint], set by base_url or OPENAI_BASE_URL, means an
// OpenAI-compatible server, not api.openai.com, and is excluded like
// elsewhere in the openai provider. An explicit api_type of
// openai_chatcompletions routes off the Responses API, which alone serves
// tool_search.
func (c *Config) NativeToolSearchEnabled() bool {
	if !catalog.IsDefaultEndpoint(c.ModelConfig.Provider, c.Endpoint()) || c.ModelConfig.ProviderOpts["api_type"] == "openai_chatcompletions" {
		return false
	}
	enabled, _ := providerutil.GetProviderOptBool(c.ModelConfig.ProviderOpts, "native_tool_search")
	return enabled && modelinfo.SupportsHostedToolSearch(c.ModelConfig.Provider, c.ModelConfig.Model)
}

// EmbeddingResult contains an embedding and its usage.
type EmbeddingResult struct {
	Embedding   []float64
	InputTokens int64
	TotalTokens int64
	Cost        float64
}

// BatchEmbeddingResult contains embeddings and their aggregate usage.
type BatchEmbeddingResult struct {
	Embeddings  [][]float64
	InputTokens int64
	TotalTokens int64
	Cost        float64
}

// Provider defines the common interface implemented by model providers.
type Provider interface {
	// ID returns a provider-qualified identity to preserve the model namespace
	// when it crosses API boundaries.
	ID() modelsdev.ID
	// CreateChatCompletionStream creates a streaming chat completion request.
	CreateChatCompletionStream(
		ctx context.Context,
		messages []chat.Message,
		tools []tools.Tool,
	) (chat.MessageStream, error)
	// BaseConfig returns the provider's base configuration.
	BaseConfig() Config
}

// ServingModel identifies the endpoint that produced a completion stream.
type ServingModel struct {
	ID     modelsdev.ID
	Config Config
}

// ServingStream reports the serving endpoint when it differs from the dispatched provider.
// The metadata is available at stream creation and remains stable after Close.
type ServingStream interface {
	chat.MessageStream
	ServingModel() ServingModel
}

// ServedStream reports, once the stream has ended, the model that served the
// response when it isn't the dispatched one (e.g. a server-side fallback).
// Wrappers must forward it rather than snapshot it at creation.
type ServedStream interface {
	chat.MessageStream
	ServedBy() (ServingModel, bool)
}

// EmbeddingProvider is a provider that supports embeddings.
type EmbeddingProvider interface {
	Provider
	CreateEmbedding(ctx context.Context, text string) (*EmbeddingResult, error)
}

// BatchEmbeddingProvider is an embedding provider that supports batches.
type BatchEmbeddingProvider interface {
	EmbeddingProvider
	// CreateBatchEmbedding returns embeddings in the same order as the inputs.
	CreateBatchEmbedding(ctx context.Context, texts []string) (*BatchEmbeddingResult, error)
}

// RerankingProvider is a provider that can score documents by relevance.
type RerankingProvider interface {
	Provider
	// Rerank returns one relevance score per document, in input order.
	Rerank(ctx context.Context, query string, documents []types.Document, criteria string) ([]float64, error)
}

// ConversationCompactor is a provider that can compact a conversation
// server-side, returning a readable summary plus an opaque block it can
// later replay in place of the compacted history.
type ConversationCompactor interface {
	Provider
	// CompactConversation compacts messages, the fully assembled prompt the
	// conversation model would receive (system messages included), with the
	// same request tools. instructions is optional user steering for the
	// summary. Returning a result with an empty Summary means nothing was
	// compacted and the caller leaves the transcript untouched.
	CompactConversation(ctx context.Context, messages []chat.Message, requestTools []tools.Tool, instructions string) (*chat.CompactionResult, error)
}
