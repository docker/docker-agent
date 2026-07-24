package base

import (
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelinfo"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

const NoDesktopTokenErrorMessage = "failed to get Docker Desktop token for Gateway. Is Docker Desktop running and are you signed in?"

// Config is a common base configuration shared by all provider clients.
// It can be embedded in provider-specific Client structs to avoid code duplication.
type Config struct {
	ModelConfig  latest.ModelConfig
	ModelOptions options.ModelOptions
	Env          environment.Provider
	// Models stores the full models map for providers that need it (e.g., routers).
	// This enables proper cloning of providers that reference other models.
	Models map[string]latest.ModelConfig
	// ProviderRegistry stores the registry that created this provider. It is kept
	// as any to avoid a package cycle; pkg/model/provider type-asserts it when
	// cloning providers with adjusted options.
	ProviderRegistry any
	// BaseURL is the resolved HTTP base URL the client talks to, when
	// the provider is reachable over a configurable HTTP endpoint.
	// Distinct from [latest.ModelConfig.BaseURL] (the user-typed input):
	// providers fill BaseURL with the URL they actually use after auto
	// discovery / fallback (e.g. Docker Model Runner picking between
	// MODEL_RUNNER_HOST, the desktop socket, and a localhost fallback).
	// Surfaced through [Config.BaseConfig] so generic, runtime-free
	// consumers like hooks can address the endpoint without duplicating
	// resolution logic. Empty for providers that don't expose a stable
	// per-instance URL.
	BaseURL string
}

func (c *Config) SetProviderRegistry(registry any) {
	c.ProviderRegistry = registry
}

// ID returns the provider and model identity as a [modelsdev.ID] so
// callers cannot accidentally pass a bare model string where a
// provider-qualified identity is required. The model component uses
// DisplayModel (the original user-configured name) when available,
// falling back to Model (the resolved/pinned name).
func (c *Config) ID() modelsdev.ID {
	return modelsdev.NewID(c.ModelConfig.Provider, c.ModelConfig.DisplayOrModel())
}

func (c *Config) BaseConfig() Config {
	return *c
}

// TrackUsageEnabled reports whether token-usage tracking is enabled for this
// model. Tracking defaults to on and is disabled only when the config
// explicitly sets track_usage: false.
func (c *Config) TrackUsageEnabled() bool {
	return c.ModelConfig.TrackUsage == nil || *c.ModelConfig.TrackUsage
}

// CapsOverride returns the model's explicit attachment-capability override
// derived from its config, or nil when the config declares none (the common
// case, in which capabilities are detected from models.dev). Provider clients
// pass the result to [modelinfo.ResolveCaps] so a user-declared override wins
// over a models.dev lookup that would otherwise miss for custom/aliased
// providers and degrade attachments to text-only (issue #2741).
//
// [latest.CapabilitiesConfig] does not yet expose audio/video override flags,
// so Audio/Video are always false here; they flow through purely from
// models.dev detection until a later config change adds them.
func (c *Config) CapsOverride() *modelinfo.CapsOverride {
	caps := c.ModelConfig.Capabilities
	if caps == nil {
		return nil
	}
	return &modelinfo.CapsOverride{Image: caps.Image, PDF: caps.PDF}
}

// EmbeddingResult contains the embedding and usage information
type EmbeddingResult struct {
	Embedding   []float64
	InputTokens int64
	TotalTokens int64
	Cost        float64
}

// BatchEmbeddingResult contains multiple embeddings and usage information
type BatchEmbeddingResult struct {
	Embeddings  [][]float64
	InputTokens int64
	TotalTokens int64
	Cost        float64
}
