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

// Cost holds base rates and optional context tiers, in USD per 1M tokens.
type Cost struct {
	Input      float64 `json:"input,omitempty"`
	Output     float64 `json:"output,omitempty"`
	CacheRead  float64 `json:"cache_read,omitempty"`
	CacheWrite float64 `json:"cache_write,omitempty"`

	Tiers []CostTier `json:"tiers,omitempty"`
}

// Rates is a single price band, in USD per 1M tokens.
type Rates struct {
	Input      float64 `json:"input,omitempty"`
	Output     float64 `json:"output,omitempty"`
	CacheRead  float64 `json:"cache_read,omitempty"`
	CacheWrite float64 `json:"cache_write,omitempty"`
}

// CostTier replaces all base rates above its threshold; omitted rates are zero.
type CostTier struct {
	Rates

	Tier TierSpec `json:"tier"`
}

// TierSpec describes the threshold above which a [CostTier] applies.
type TierSpec struct {
	// Type is the tier dimension; models.dev currently only defines "context".
	Type string `json:"type,omitempty"`
	// Size is the exclusive prompt-token threshold.
	Size int64 `json:"size"`
}

// RatesFor selects the highest threshold exceeded by the prompt, or the base rates.
// Prompt tokens include cache reads and writes, but not output tokens.
func (c *Cost) RatesFor(promptTokens int64) Rates {
	var best *CostTier
	for i := range c.Tiers {
		t := &c.Tiers[i]
		if t.Tier.Type != "" && t.Tier.Type != "context" {
			continue
		}
		if promptTokens > t.Tier.Size && (best == nil || t.Tier.Size > best.Tier.Size) {
			best = t
		}
	}
	if best != nil {
		return best.Rates
	}
	return Rates{Input: c.Input, Output: c.Output, CacheRead: c.CacheRead, CacheWrite: c.CacheWrite}
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
