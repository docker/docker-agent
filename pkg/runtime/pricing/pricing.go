// Package pricing calculates per-call costs without mutating catalog entries.
package pricing

import (
	"strings"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

// ApplyModelCost resolves configured and serving-tier rates for a call.
func ApplyModelCost(m *modelsdev.Model, id modelsdev.ID, usage *chat.Usage, config base.Config) *modelsdev.Model {
	cfg := config.ModelConfig
	// Unreported routing endpoints and custom endpoints cannot use vendor tier multipliers.
	isCustomEndpoint := func(endpoint string) bool {
		return endpoint != "" && strings.TrimRight(endpoint, "/") != "https://api.openai.com/v1"
	}
	customEndpoint := isCustomEndpoint(cfg.BaseURL) || isCustomEndpoint(config.BaseURL)
	if cfg.Cost != nil || customEndpoint || len(cfg.Routing) > 0 || m == nil || usage == nil {
		return ApplyConfigCost(m, id, cfg.Cost)
	}
	cost := m.Cost.ForServiceTier(id, usage.ServiceTier)
	if cost == m.Cost {
		return m
	}
	out := *m
	out.Cost = cost
	return &out
}

// ApplyConfigCost overlays a config-declared price table (USD per 1M tokens)
// onto the catalogue entry, returning m untouched when there is no override.
// It never mutates m: the store caches entries shared across sessions. When
// the model is uncatalogued (m == nil) a minimal entry is synthesized so the
// call is still priced — this is the escape hatch for custom endpoints that
// models.dev cannot describe.
func ApplyConfigCost(m *modelsdev.Model, id modelsdev.ID, cost *latest.CostConfig) *modelsdev.Model {
	if cost == nil {
		return m
	}
	var out modelsdev.Model
	if m != nil {
		out = *m
	} else {
		out.Name = id.Model
	}
	out.Cost = &modelsdev.Cost{
		Input:      cost.Input,
		Output:     cost.Output,
		CacheRead:  cost.CacheRead,
		CacheWrite: cost.CacheWrite,
	}
	return &out
}

// ComputeMessageCost prices the whole call at the tier selected by its prompt size.
// Nil means unpriced; a non-nil zero means free. Messages and hooks share this value.
func ComputeMessageCost(usage *chat.Usage, m *modelsdev.Model) *float64 {
	if usage == nil || m == nil || m.Cost == nil {
		return nil
	}
	rates := m.Cost.RatesFor(usage.PromptTokens())
	return new((float64(usage.InputTokens)*rates.Input +
		float64(usage.OutputTokens)*rates.Output +
		float64(usage.CachedInputTokens)*rates.CacheRead +
		float64(usage.CacheWriteTokens)*rates.CacheWrite) / 1e6)
}
