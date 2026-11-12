package modelsdev

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCatalogCostCorrectionScope(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		provider, model, dimension string
		size                       int64
	}{
		{"vercel", "openai/gpt-6-astra", "context", 272_000},
		{"vercel", "openai/gpt-6-astra", "context", 200_000},
		{"vercel", "openai/gpt-6-astra", "other", 272_001},
		{"openai", "gpt-6-astra", "context", 272_001},
		{"vercel", "openai/gpt-5.6-luna", "context", 272_001},
	} {
		id := NewID(tc.provider, tc.model)
		cost := &Cost{Input: 10, Tiers: []CostTier{{Rates: Rates{Input: 20}, Tier: TierSpec{Type: tc.dimension, Size: tc.size}}}}
		assert.Same(t, cost, cost.forModel(id))
	}
	var missing *Cost
	assert.Nil(t, missing.forModel(NewID("vercel", "openai/gpt-6-astra")))
}
