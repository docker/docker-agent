package base

import (
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/options"
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
