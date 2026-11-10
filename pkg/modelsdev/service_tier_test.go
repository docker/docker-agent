package modelsdev

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCostForServiceTier(t *testing.T) {
	t.Parallel()

	cost := &Cost{
		Input: 10, Output: 50, CacheRead: 1, CacheWrite: 12.5,
		Tiers: []CostTier{{
			Rates: Rates{Input: 20, Output: 75, CacheRead: 2, CacheWrite: 25},
			Tier:  TierSpec{Type: "context", Size: 272_000},
		}},
	}
	for _, tier := range []string{"fast", "priority", "ultrafast"} {
		t.Run(tier, func(t *testing.T) {
			t.Parallel()
			factor := 2.0
			if tier == "ultrafast" {
				factor = 6
			}
			got := cost.ForServiceTier(NewID("openai", "gpt-6-astra"), tier)
			require.NotSame(t, cost, got)
			assert.Equal(t, Rates{Input: 10 * factor, Output: 50 * factor, CacheRead: factor, CacheWrite: 12.5 * factor}, got.RatesFor(272_000))
			assert.Equal(t, Rates{Input: 20 * factor, Output: 75 * factor, CacheRead: 2 * factor, CacheWrite: 25 * factor}, got.RatesFor(272_001))
			assert.Equal(t, cost.Tiers[0].Tier, got.Tiers[0].Tier)
			assert.Equal(t, Rates{Input: 10, Output: 50, CacheRead: 1, CacheWrite: 12.5}, cost.RatesFor(272_000))
			assert.Equal(t, Rates{Input: 20, Output: 75, CacheRead: 2, CacheWrite: 25}, cost.RatesFor(272_001), "shared catalogue tiers must not be mutated")
		})
	}
}

func TestCostForServiceTierAnthropicFast(t *testing.T) {
	t.Parallel()

	cost := &Cost{
		Input: 4, Output: 20, CacheRead: 0.4, CacheWrite: 5,
		Tiers: []CostTier{{
			Rates: Rates{Input: 8, Output: 30, CacheRead: 0.8, CacheWrite: 10},
			Tier:  TierSpec{Type: "context", Size: 200_000},
		}},
	}
	got := cost.ForServiceTier(NewID("anthropic", "claude-opus-5-5"), "fast")

	require.NotSame(t, cost, got)
	assert.Equal(t, Rates{Input: 8, Output: 40, CacheRead: 0.8, CacheWrite: 10}, got.RatesFor(200_000))
	assert.Equal(t, Rates{Input: 16, Output: 60, CacheRead: 1.6, CacheWrite: 20}, got.RatesFor(200_001))
	assert.Equal(t, Rates{Input: 4, Output: 20, CacheRead: 0.4, CacheWrite: 5}, cost.RatesFor(200_000), "shared catalogue must not be mutated")
}

func TestCostForServiceTierIndependentOfModel(t *testing.T) {
	t.Parallel()

	cost := &Cost{Input: 1}
	for _, model := range []string{"gpt-6.1-sol", "gpt-6-astra-2026-09-04", "future-model", "future-model-fast"} {
		for _, tc := range []struct {
			tier   string
			factor float64
		}{{"fast", 2}, {"priority", 2}, {"ultrafast", 6}} {
			assert.InDelta(t, tc.factor, cost.ForServiceTier(NewID("openai", model), tc.tier).Input, 1e-9, "%s/%s", model, tc.tier)
		}
	}
}

func TestCostForServiceTierUnchanged(t *testing.T) {
	t.Parallel()

	cost := &Cost{Input: 1}
	for _, tc := range []struct {
		provider, model, tier string
	}{
		{"openai", "gpt-6-astra", ""},
		{"openai", "gpt-6-astra", "default"},
		{"openai", "gpt-6-astra", "auto"},
		{"openai", "gpt-6-astra", "flex"},
		{"openai", "gpt-6-astra", "scale"},
		{"openai", "gpt-6-astra", "future-tier"},
		{"vercel", "openai/gpt-6-astra-fast", "fast"},
		{"azure", "gpt-6-astra", "fast"},
		{"chatgpt", "gpt-6-astra", "ultrafast"},
		{"custom", "gpt-6-astra", "fast"},
		{"anthropic", "claude-opus-5-5", ""},
		{"anthropic", "claude-opus-5-5", "standard"},
		{"anthropic", "claude-opus-5-5", "priority"},
		{"anthropic", "claude-opus-5-5", "ultrafast"},
		{"google", "claude-opus-5-5", "fast"},
	} {
		assert.Same(t, cost, cost.ForServiceTier(NewID(tc.provider, tc.model), tc.tier), "%+v", tc)
	}
	var missing *Cost
	assert.Nil(t, missing.ForServiceTier(NewID("openai", "gpt-6-astra"), "ultrafast"))
}

func TestCatalogFastPremium(t *testing.T) {
	t.Parallel()

	base := &Cost{
		Input: 2, Output: 10, CacheRead: 0.2, CacheWrite: 2.5,
		Tiers: []CostTier{{Rates: Rates{Input: 4, Output: 15, CacheRead: 0.4, CacheWrite: 5}, Tier: TierSpec{Type: "context", Size: 272_000}}},
	}
	for _, tc := range []struct {
		name     string
		standard *Cost
		fast     *Cost
		factor   float64
	}{
		{name: "uniform premium", standard: base, fast: &Cost{Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25}, factor: 2.5},
		{name: "missing fast", standard: base, factor: 2},
		{name: "missing standard", fast: &Cost{Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25}, factor: 2},
		{name: "different gateway base rates", standard: &Cost{Input: 1}, fast: &Cost{Input: 3}, factor: 2},
		{name: "different token premiums", standard: base, fast: &Cost{Input: 5, Output: 20, CacheRead: 0.5, CacheWrite: 6.25}, factor: 2},
		{name: "inconsistent context premium", standard: base, fast: &Cost{
			Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25,
			Tiers: []CostTier{{Rates: Rates{Input: 8, Output: 30, CacheRead: 0.8, CacheWrite: 10}, Tier: TierSpec{Type: "context", Size: 272_001}}},
		}, factor: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := NewDatabaseStore(&Database{Providers: map[string]Provider{
				"openai": {Models: map[string]Model{"future-model": {Cost: base}, "future-model-2026-10-17": {Cost: base}}},
				"vercel": {Models: map[string]Model{"openai/future-model": {Cost: tc.standard}, "openai/future-model-fast": {Cost: tc.fast}}},
			}})
			for _, name := range []string{"future-model", "future-model-2026-10-17"} {
				id := NewID("openai", name)
				model, err := store.GetModel(t.Context(), id)
				require.NoError(t, err)
				for _, tier := range []string{"fast", "priority"} {
					priced := model.Cost.ForServiceTier(id, tier)
					assert.InDelta(t, 2*tc.factor, priced.RatesFor(100).Input, 1e-9)
					assert.InDelta(t, 4*tc.factor, priced.RatesFor(300_000).Input, 1e-9)
				}
				assert.InDelta(t, 12, model.Cost.ForServiceTier(id, "ultrafast").Input, 1e-9)
				assert.Same(t, model.Cost, model.Cost.ForServiceTier(id, "default"))
				assert.Zero(t, base.fastMultiplier, "catalogue metadata must not be mutated")
			}
		})
	}
}
