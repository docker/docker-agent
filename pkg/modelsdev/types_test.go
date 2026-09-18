package modelsdev

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCostRatesFor(t *testing.T) {
	t.Parallel()

	base := Rates{Input: 2.5, Output: 15, CacheRead: 0.25}
	over272k := Rates{Input: 5, Output: 22.5, CacheRead: 0.5}
	flat := &Cost{Input: base.Input, Output: base.Output, CacheRead: base.CacheRead}
	tiered := &Cost{
		Input: base.Input, Output: base.Output, CacheRead: base.CacheRead,
		Tiers: []CostTier{{Rates: over272k, Tier: TierSpec{Type: "context", Size: 272_000}}},
	}

	t.Run("no tiers always yields the base band", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, base, flat.RatesFor(0))
		assert.Equal(t, base, flat.RatesFor(1_000_000))
	})

	t.Run("prompt at or below the threshold yields the base band", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, base, tiered.RatesFor(0))
		assert.Equal(t, base, tiered.RatesFor(200_000))
		assert.Equal(t, base, tiered.RatesFor(272_000), "the threshold itself is not 'over'")
	})

	t.Run("prompt above the threshold yields the tier", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, over272k, tiered.RatesFor(272_001))
		assert.Equal(t, over272k, tiered.RatesFor(1_000_000))
	})

	t.Run("highest exceeded tier wins regardless of order", func(t *testing.T) {
		t.Parallel()
		low := Rates{Input: 0.13, Output: 0.8}
		high := Rates{Input: 0.27, Output: 1.6}
		c := &Cost{
			Input: 0.09, Output: 0.53,
			Tiers: []CostTier{
				{Rates: high, Tier: TierSpec{Size: 128_000}},
				{Rates: low, Tier: TierSpec{Size: 32_000}},
			},
		}
		assert.Equal(t, Rates{Input: 0.09, Output: 0.53}, c.RatesFor(32_000))
		assert.Equal(t, low, c.RatesFor(32_001))
		assert.Equal(t, low, c.RatesFor(128_000))
		assert.Equal(t, high, c.RatesFor(128_001))
	})

	t.Run("tier omitting a rate does not inherit it from the base band", func(t *testing.T) {
		t.Parallel()
		c := &Cost{
			Input: 1, Output: 2, CacheRead: 0.1, CacheWrite: 0.5,
			Tiers: []CostTier{{Rates: Rates{Input: 2, Output: 4}, Tier: TierSpec{Size: 100}}},
		}
		assert.Equal(t, Rates{Input: 2, Output: 4}, c.RatesFor(101))
	})

	t.Run("zero-rate tier is free rather than falling back", func(t *testing.T) {
		t.Parallel()
		c := &Cost{
			Input: 1, Output: 2,
			Tiers: []CostTier{{Tier: TierSpec{Type: "context", Size: 200_000}}},
		}
		assert.Equal(t, Rates{}, c.RatesFor(200_001))
	})

	t.Run("non-context tiers are ignored", func(t *testing.T) {
		t.Parallel()
		c := &Cost{
			Input: 1, Output: 2,
			Tiers: []CostTier{{Rates: Rates{Input: 9, Output: 9}, Tier: TierSpec{Type: "batch", Size: 0}}},
		}
		assert.Equal(t, Rates{Input: 1, Output: 2}, c.RatesFor(1_000_000))
	})
}

func TestCostJSONRoundTrip(t *testing.T) {
	t.Parallel()

	const wire = `{"input":2.5,"output":15,"cache_read":0.25,` +
		`"tiers":[{"input":5,"output":22.5,"cache_read":0.5,"tier":{"type":"context","size":272000}}],` +
		`"context_over_200k":{"input":5,"output":22.5,"cache_read":0.5}}`

	var c Cost
	require.NoError(t, json.Unmarshal([]byte(wire), &c))
	require.Len(t, c.Tiers, 1)
	assert.Equal(t, CostTier{
		Rates: Rates{Input: 5, Output: 22.5, CacheRead: 0.5},
		Tier:  TierSpec{Type: "context", Size: 272_000},
	}, c.Tiers[0])

	out, err := json.Marshal(c)
	require.NoError(t, err)
	assert.JSONEq(t, `{"input":2.5,"output":15,"cache_read":0.25,`+
		`"tiers":[{"input":5,"output":22.5,"cache_read":0.5,"tier":{"type":"context","size":272000}}]}`,
		string(out), "the legacy context_over_200k alias is dropped; tiers are authoritative")
}
