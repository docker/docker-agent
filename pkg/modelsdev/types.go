package modelsdev

import "time"

// Database represents the complete models.dev database
type Database struct {
	Providers map[string]Provider `json:"providers"`
}

// Provider represents an AI model provider
type Provider struct {
	Models map[string]Model `json:"models"`
}

// Model represents an AI model with its specifications and capabilities.
//
// Fields are sourced from https://models.dev/api.json. Boolean capability
// fields default to false when absent from the source data.
type Model struct {
	Name       string     `json:"name"`
	Family     string     `json:"family,omitempty"`
	Cost       *Cost      `json:"cost,omitempty"`
	Limit      Limit      `json:"limit"`
	Modalities Modalities `json:"modalities"`

	// Reasoning is true when the model supports internal reasoning.
	Reasoning bool `json:"reasoning,omitempty"`
	// ToolCall is true when the model supports tool/function calls.
	ToolCall bool `json:"tool_call,omitempty"`
	// Temperature is true when the API accepts the temperature parameter.
	Temperature bool `json:"temperature,omitempty"`
	// Attachment is true when the model accepts file/image attachments.
	Attachment bool `json:"attachment,omitempty"`
	// OpenWeights is true when the model has openly-released weights.
	OpenWeights bool `json:"open_weights,omitempty"`
	// ReleaseDate is the model's public release date (YYYY-MM-DD).
	ReleaseDate string `json:"release_date,omitempty"`
}

// Cost represents the pricing information for a model
type Cost struct {
	Input      float64 `json:"input,omitempty"`
	Output     float64 `json:"output,omitempty"`
	CacheRead  float64 `json:"cache_read,omitempty"`
	CacheWrite float64 `json:"cache_write,omitempty"`
}

// Limit represents the context and output limitations of a model
type Limit struct {
	Context int   `json:"context"`
	Output  int64 `json:"output"`
}

// Modalities represents the supported input and output types
type Modalities struct {
	Input  []string `json:"input"`
	Output []string `json:"output"`
}

// CachedData represents the cached models.dev data with metadata
type CachedData struct {
	Database    Database  `json:"database"`
	LastRefresh time.Time `json:"last_refresh"`
	ETag        string    `json:"etag,omitempty"`
}
