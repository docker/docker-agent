package runtime

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime/pricing"
)

// TestUsageHasTokens covers the helper that suppresses the missing-price
// warning for empty/no-op turns. The per-message cost arithmetic and its
// nil/unpriced branches are exercised by TestComputeMessageCost in
// after_llm_call_test.go, which shares the same pricing.ComputeMessageCost source.
func TestUsageHasTokens(t *testing.T) {
	t.Parallel()
	assert.False(t, usageHasTokens(nil), "nil usage has no tokens")
	assert.False(t, usageHasTokens(&chat.Usage{}), "zero usage has no tokens")
	assert.True(t, usageHasTokens(&chat.Usage{InputTokens: 1}))
	assert.True(t, usageHasTokens(&chat.Usage{OutputTokens: 1}))
	assert.True(t, usageHasTokens(&chat.Usage{CachedInputTokens: 1}))
	assert.True(t, usageHasTokens(&chat.Usage{CacheWriteTokens: 1}))
}

// TestApplyConfigCost pins the config price-table override: it must take
// precedence over the catalogue, price uncatalogued models, never mutate the
// (shared, store-cached) catalogue entry, and be a no-op when unset.
func TestApplyConfigCost(t *testing.T) {
	t.Parallel()

	id := modelsdev.NewID("custom", "my-model")
	usage := &chat.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000}

	t.Run("nil override is a no-op", func(t *testing.T) {
		t.Parallel()
		m := &modelsdev.Model{Cost: &modelsdev.Cost{Input: 3}}
		assert.Same(t, m, pricing.ApplyConfigCost(m, id, nil))
		assert.Nil(t, pricing.ApplyConfigCost(nil, id, nil))
	})

	t.Run("override replaces the catalogue price without mutating it", func(t *testing.T) {
		t.Parallel()
		catalogued := &modelsdev.Model{Name: "My Model", Cost: &modelsdev.Cost{Input: 3, Output: 15}}
		m := pricing.ApplyConfigCost(catalogued, id, &latest.CostConfig{Input: 1.25, Output: 5})

		cost := pricing.ComputeMessageCost(usage, m)
		require.NotNil(t, cost)
		assert.InEpsilon(t, 6.25, *cost, 0.0001)
		assert.Equal(t, "My Model", m.Name, "catalogue metadata must be preserved")
		assert.InEpsilon(t, 3.0, catalogued.Cost.Input, 0.0001, "the cached catalogue entry must not be mutated")
	})

	t.Run("override prices an uncatalogued model", func(t *testing.T) {
		t.Parallel()
		m := pricing.ApplyConfigCost(nil, id, &latest.CostConfig{Input: 2, Output: 4})

		cost := pricing.ComputeMessageCost(usage, m)
		require.NotNil(t, cost, "a config cost must make an uncatalogued model priced")
		assert.InEpsilon(t, 6.0, *cost, 0.0001)
		assert.Equal(t, "my-model", m.Name, "synthesized entry carries the model name for telemetry")
	})

	t.Run("all-zero override is priced free, not unpriced", func(t *testing.T) {
		t.Parallel()
		m := pricing.ApplyConfigCost(nil, id, &latest.CostConfig{})

		cost := pricing.ComputeMessageCost(usage, m)
		require.NotNil(t, cost, "a zero price table still counts as priced")
		assert.Zero(t, *cost)
	})
}

func TestComputeMessageCostContextTiers(t *testing.T) {
	t.Parallel()

	model := &modelsdev.Model{Cost: &modelsdev.Cost{
		Input: 1, Output: 2, CacheRead: 0.1, CacheWrite: 1.25,
		Tiers: []modelsdev.CostTier{{
			Rates: modelsdev.Rates{Input: 3, Output: 4, CacheRead: 0.3, CacheWrite: 3.75},
			Tier:  modelsdev.TierSpec{Type: "context", Size: 200_000},
		}},
	}}
	for _, tc := range []struct {
		name  string
		usage chat.Usage
		want  float64
	}{
		{
			name:  "output and reasoning do not select the tier",
			usage: chat.Usage{InputTokens: 200_000, OutputTokens: 100_000, ReasoningTokens: 50_000},
			want:  0.4,
		},
		{
			name:  "fresh input selects the tier",
			usage: chat.Usage{InputTokens: 200_001},
			want:  200_001 * 3 / 1e6,
		},
		{
			name:  "cache writes cross the threshold and use tier rates",
			usage: chat.Usage{InputTokens: 100_000, CachedInputTokens: 50_000, CacheWriteTokens: 50_001, OutputTokens: 1_000},
			want:  (100_000*3 + 50_000*0.3 + 50_001*3.75 + 1_000*4) / 1e6,
		},
		{
			name:  "empty usage is free",
			usage: chat.Usage{},
			want:  0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := pricing.ComputeMessageCost(&tc.usage, model)
			require.NotNil(t, got)
			assert.InDelta(t, tc.want, *got, 1e-9)
		})
	}
}

func TestConfigCostReplacesContextTiers(t *testing.T) {
	t.Parallel()

	catalogued := &modelsdev.Model{Cost: &modelsdev.Cost{
		Input: 2,
		Tiers: []modelsdev.CostTier{{
			Rates: modelsdev.Rates{Input: 4},
			Tier:  modelsdev.TierSpec{Type: "context", Size: 200_000},
		}},
	}}
	usage := &chat.Usage{InputTokens: 1_000_000}
	for _, price := range []float64{0, 1.25} {
		m := pricing.ApplyConfigCost(catalogued, modelsdev.NewID("openai", "test"), &latest.CostConfig{Input: price})
		assert.Empty(t, m.Cost.Tiers)
		got := pricing.ComputeMessageCost(usage, m)
		require.NotNil(t, got)
		assert.InDelta(t, price, *got, 1e-9)
	}
	got := pricing.ComputeMessageCost(usage, catalogued)
	require.NotNil(t, got)
	assert.InDelta(t, 4.0, *got, 1e-9, "overrides must not mutate shared catalog tiers")
}

func TestApplyModelCostServiceTier(t *testing.T) {
	t.Parallel()

	store := modelsdev.NewDatabaseStore(modelsdev.EmbeddedSnapshot())
	for _, tc := range []struct {
		name, provider, model, tier string
		cfg                         latest.ModelConfig
		short, long                 float64
	}{
		{name: "GPT-4o fast", provider: "openai", model: "gpt-4o", tier: "fast", short: 0.34, long: 2.2525},
		{name: "dated GPT-4o priority", provider: "openai", model: "gpt-4o-2024-08-06", tier: "priority", short: 0.34, long: 2.2525},
		{name: "GPT-5.5 fast", provider: "openai", model: "gpt-5.5", tier: "fast", short: 1.025, long: 13.1125},
		{name: "GPT-5.5 downgrade", provider: "openai", model: "gpt-5.5", tier: "default", cfg: latest.ModelConfig{ProviderOpts: map[string]any{"service_tier": "fast"}}, short: 0.41, long: 5.245},
		{name: "Luna fast", provider: "openai", model: "gpt-5.6-luna", tier: "fast", short: 0.0478, long: 0.4496},
		{name: "Luna priority alias", provider: "openai", model: "gpt-5.6-luna", tier: "priority", short: 0.0478, long: 0.4496},
		{name: "Astra fast", provider: "openai", model: "gpt-6-astra", tier: "fast", short: 2.29, long: 22.33},
		{name: "Astra ultrafast", provider: "openai", model: "gpt-6-astra", tier: "ultrafast", short: 6.87, long: 66.99},
		{name: "actual default after fast request", provider: "openai", model: "gpt-6-astra", tier: "default", cfg: latest.ModelConfig{ProviderOpts: map[string]any{"service_tier": "fast"}}, short: 1.145, long: 11.165},
		{name: "actual fast without requested tier", provider: "openai", model: "gpt-6-astra", tier: "fast", short: 2.29, long: 22.33},
		{name: "missing actual tier", provider: "openai", model: "gpt-6-astra", cfg: latest.ModelConfig{ProviderOpts: map[string]any{"service_tier": "ultrafast"}}, short: 1.145, long: 11.165},
		{name: "unknown tier", provider: "openai", model: "gpt-6-astra", tier: "future-tier", short: 1.145, long: 11.165},
		{name: "Luna reported ultrafast", provider: "openai", model: "gpt-5.6-luna", tier: "ultrafast", short: 0.1434, long: 1.3488},
		{name: "Sol 6.1 fast", provider: "openai", model: "gpt-6.1-sol", tier: "fast", short: 0.454, long: 4.458},
		{name: "Sol 6.1 ultrafast", provider: "openai", model: "gpt-6.1-sol", tier: "ultrafast", short: 1.362, long: 13.374},
		{name: "Sol 6.1 downgraded to fast", provider: "openai", model: "gpt-6.1-sol", tier: "fast", cfg: latest.ModelConfig{ProviderOpts: map[string]any{"service_tier": "ultrafast"}}, short: 0.454, long: 4.458},
		{name: "Sol 6.1 downgraded to default", provider: "openai", model: "gpt-6.1-sol", tier: "default", cfg: latest.ModelConfig{ProviderOpts: map[string]any{"service_tier": "ultrafast"}}, short: 0.227, long: 2.229},
		{name: "Sol 6.1 missing actual tier", provider: "openai", model: "gpt-6.1-sol", cfg: latest.ModelConfig{ProviderOpts: map[string]any{"service_tier": "ultrafast"}}, short: 0.227, long: 2.229},
		{name: "gateway fast already priced", provider: "vercel", model: "openai/gpt-6-astra-fast", tier: "fast", short: 2.29, long: 22.33},
		{name: "Azure unchanged", provider: "azure", model: "gpt-6-astra", tier: "fast", short: 1.145, long: 11.165},
		{name: "custom OpenAI endpoint unchanged", provider: "openai", model: "gpt-6-astra", tier: "ultrafast", cfg: latest.ModelConfig{BaseURL: "https://example.com/v1"}, short: 1.145, long: 11.165},
		{name: "override wins over ultrafast and context bands", provider: "openai", model: "gpt-6-astra", tier: "ultrafast", cfg: latest.ModelConfig{Cost: &latest.CostConfig{Input: 1, Output: 2, CacheRead: 0.1, CacheWrite: 1.25}}, short: 0.0995, long: 0.5495},
		{name: "zero override stays free", provider: "openai", model: "gpt-6-astra", tier: "fast", cfg: latest.ModelConfig{Cost: &latest.CostConfig{}}, short: 0, long: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			id := modelsdev.NewID(tc.provider, tc.model)
			model, err := store.GetModel(t.Context(), id)
			require.NoError(t, err)
			original := *model.Cost
			original.Tiers = slices.Clone(original.Tiers)
			for _, band := range []struct {
				input int64
				want  float64
			}{{50_000, tc.short}, {500_000, tc.long}} {
				usage := &chat.Usage{InputTokens: band.input, CachedInputTokens: 20_000, CacheWriteTokens: 30_000, OutputTokens: 5_000, ReasoningTokens: 3_000, ServiceTier: tc.tier}
				priced := pricing.ApplyModelCost(model, id, usage, base.Config{ModelConfig: tc.cfg})
				got := pricing.ComputeMessageCost(usage, priced)
				require.NotNil(t, got)
				assert.InDelta(t, band.want, *got, 1e-9)
				assert.Equal(t, original, *model.Cost, "shared catalogue must not be mutated")
			}
		})
	}
}

func TestApplyModelCostServiceTierUnpriced(t *testing.T) {
	t.Parallel()

	id := modelsdev.NewID("openai", "gpt-6-astra")
	usage := &chat.Usage{InputTokens: 1_000_000, ServiceTier: "ultrafast"}
	assert.Nil(t, pricing.ApplyModelCost(nil, id, usage, base.Config{}))
	model := &modelsdev.Model{}
	assert.Same(t, model, pricing.ApplyModelCost(model, id, usage, base.Config{}))
	assert.Same(t, model, pricing.ApplyModelCost(model, id, nil, base.Config{}))
	priced := pricing.ApplyModelCost(nil, id, usage, base.Config{ModelConfig: latest.ModelConfig{Cost: &latest.CostConfig{Input: 1.25}}})
	cost := pricing.ComputeMessageCost(usage, priced)
	require.NotNil(t, cost)
	assert.InDelta(t, 1.25, *cost, 1e-9)
}

func TestApplyModelCostEndpointEligibility(t *testing.T) {
	t.Parallel()

	id := modelsdev.NewID("openai", "gpt-6-astra")
	usage := &chat.Usage{InputTokens: 1_000_000, ServiceTier: "ultrafast"}
	model := &modelsdev.Model{Cost: &modelsdev.Cost{Input: 10}}
	for _, tc := range []struct {
		name   string
		config base.Config
		want   float64
	}{
		{name: "official resolved endpoint", config: base.Config{BaseURL: "https://api.openai.com/v1/"}, want: 60},
		{name: "official configured endpoint", config: base.Config{ModelConfig: latest.ModelConfig{BaseURL: "https://api.openai.com/v1"}}, want: 60},
		{name: "official configured endpoint trailing slash", config: base.Config{ModelConfig: latest.ModelConfig{BaseURL: "https://api.openai.com/v1/"}, BaseURL: "https://api.openai.com/v1/"}, want: 60},
		{name: "custom configured endpoint", config: base.Config{ModelConfig: latest.ModelConfig{BaseURL: "https://example.com/v1"}}, want: 10},
		{name: "gateway with custom configured endpoint", config: base.Config{ModelConfig: latest.ModelConfig{BaseURL: "https://example.com/v1"}, BaseURL: "https://api.openai.com/v1"}, want: 10},
		{name: "resolved custom endpoint wins", config: base.Config{ModelConfig: latest.ModelConfig{BaseURL: "https://api.openai.com/v1"}, BaseURL: "https://example.com/v1"}, want: 10},
		{name: "environment redirected endpoint", config: base.Config{BaseURL: "https://example.com/v1"}, want: 10},
		{name: "router endpoint unknown", config: base.Config{ModelConfig: latest.ModelConfig{Routing: []latest.RoutingRule{{Model: "custom"}}}}, want: 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cost := pricing.ComputeMessageCost(usage, pricing.ApplyModelCost(model, id, usage, tc.config))
			require.NotNil(t, cost)
			assert.InDelta(t, tc.want, *cost, 1e-9)
		})
	}
}

func TestPublishedCostBoundary(t *testing.T) {
	t.Parallel()

	store := modelsdev.NewDatabaseStore(modelsdev.EmbeddedSnapshot())
	// https://developers.openai.com/api/docs/pricing: USD per million tokens.
	for _, tc := range []struct {
		name, provider, model, tier string
		short, long                 modelsdev.Rates
	}{
		{"Luna standard", "openai", "gpt-5.6-luna", "default", modelsdev.Rates{Input: 0.2, CacheRead: 0.02, CacheWrite: 0.25, Output: 1.2}, modelsdev.Rates{Input: 0.4, CacheRead: 0.04, CacheWrite: 0.5, Output: 1.8}},
		{"Luna fast", "openai", "gpt-5.6-luna", "fast", modelsdev.Rates{Input: 0.4, CacheRead: 0.04, CacheWrite: 0.5, Output: 2.4}, modelsdev.Rates{Input: 0.8, CacheRead: 0.08, CacheWrite: 1, Output: 3.6}},
		{"Luna priority", "openai", "gpt-5.6-luna", "priority", modelsdev.Rates{Input: 0.4, CacheRead: 0.04, CacheWrite: 0.5, Output: 2.4}, modelsdev.Rates{Input: 0.8, CacheRead: 0.08, CacheWrite: 1, Output: 3.6}},
		{"Astra standard", "openai", "gpt-6-astra", "default", modelsdev.Rates{Input: 10, CacheRead: 1, CacheWrite: 12.5, Output: 50}, modelsdev.Rates{Input: 20, CacheRead: 2, CacheWrite: 25, Output: 75}},
		{"Astra fast", "openai", "gpt-6-astra", "fast", modelsdev.Rates{Input: 20, CacheRead: 2, CacheWrite: 25, Output: 100}, modelsdev.Rates{Input: 40, CacheRead: 4, CacheWrite: 50, Output: 150}},
		{"Astra priority", "openai", "gpt-6-astra", "priority", modelsdev.Rates{Input: 20, CacheRead: 2, CacheWrite: 25, Output: 100}, modelsdev.Rates{Input: 40, CacheRead: 4, CacheWrite: 50, Output: 150}},
		{"Astra ultrafast", "openai", "gpt-6-astra", "ultrafast", modelsdev.Rates{Input: 60, CacheRead: 6, CacheWrite: 75, Output: 300}, modelsdev.Rates{Input: 120, CacheRead: 12, CacheWrite: 150, Output: 450}},
		{"Sol 6.1 standard", "openai", "gpt-6.1-sol", "default", modelsdev.Rates{Input: 2, CacheRead: 0.1, CacheWrite: 2.5, Output: 10}, modelsdev.Rates{Input: 4, CacheRead: 0.2, CacheWrite: 5, Output: 15}},
		{"Sol 6.1 fast", "openai", "gpt-6.1-sol", "fast", modelsdev.Rates{Input: 4, CacheRead: 0.2, CacheWrite: 5, Output: 20}, modelsdev.Rates{Input: 8, CacheRead: 0.4, CacheWrite: 10, Output: 30}},
		{"Sol 6.1 ultrafast", "openai", "gpt-6.1-sol", "ultrafast", modelsdev.Rates{Input: 12, CacheRead: 0.6, CacheWrite: 15, Output: 60}, modelsdev.Rates{Input: 24, CacheRead: 1.2, CacheWrite: 30, Output: 90}},
		{"gateway Astra", "vercel", "openai/gpt-6-astra", "default", modelsdev.Rates{Input: 10, CacheRead: 1, CacheWrite: 12.5, Output: 50}, modelsdev.Rates{Input: 20, CacheRead: 2, CacheWrite: 25, Output: 75}},
		{"gateway Astra fast", "vercel", "openai/gpt-6-astra-fast", "fast", modelsdev.Rates{Input: 20, CacheRead: 2, CacheWrite: 25, Output: 100}, modelsdev.Rates{Input: 40, CacheRead: 4, CacheWrite: 50, Output: 150}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			id := modelsdev.NewID(tc.provider, tc.model)
			model, err := store.GetModel(t.Context(), id)
			require.NoError(t, err)
			for _, prompt := range []int64{271_999, 272_000, 272_001, 272_002} {
				for _, bucket := range []string{"fresh", "read", "write"} {
					usage := &chat.Usage{InputTokens: 72_000, CachedInputTokens: 100_000, CacheWriteTokens: 100_000, OutputTokens: 10_000, ReasoningTokens: 9_000, ServiceTier: tc.tier}
					switch bucket {
					case "fresh":
						usage.InputTokens += prompt - 272_000
					case "read":
						usage.CachedInputTokens += prompt - 272_000
					case "write":
						usage.CacheWriteTokens += prompt - 272_000
					}
					rates := tc.short
					if prompt > 272_000 {
						rates = tc.long
					}
					want := (float64(usage.InputTokens)*rates.Input + float64(usage.CachedInputTokens)*rates.CacheRead + float64(usage.CacheWriteTokens)*rates.CacheWrite + 10_000*rates.Output) / 1e6
					got := pricing.ComputeMessageCost(usage, pricing.ApplyModelCost(model, id, usage, base.Config{}))
					require.NotNil(t, got)
					assert.InDelta(t, want, *got, 1e-9, "prompt=%d, varying %s tokens", prompt, bucket)
				}
			}
		})
	}
}
