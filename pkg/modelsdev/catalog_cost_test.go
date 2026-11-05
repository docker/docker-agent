package modelsdev

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVercelAstraContextBoundary(t *testing.T) {
	t.Parallel()

	db := EmbeddedSnapshot()
	for _, modelID := range []string{"openai/gpt-6-astra", "openai/gpt-6-astra-fast"} {
		t.Run(modelID, func(t *testing.T) {
			t.Parallel()
			id := NewID("vercel", modelID)
			source := db.Providers[id.Provider].Models[id.Model].Cost
			require.NotNil(t, source)
			original := *source
			original.Tiers = slices.Clone(source.Tiers)
			for _, path := range []string{"embedded", "cache", "fetch"} {
				t.Run(path, func(t *testing.T) {
					t.Parallel()
					store := NewDatabaseStore(db)
					if path != "embedded" {
						cache := filepath.Join(t.TempDir(), "models.json")
						if path == "cache" {
							writeCache(t, cache, *db)
						}
						var err error
						store, err = NewStore(WithCache(cache), WithOffline(false), WithFetcher(func(context.Context, string) (*Database, string, error) {
							assert.Equal(t, "fetch", path, "fresh cache must not fetch")
							return db, "etag", nil
						}))
						require.NoError(t, err)
					}
					model, err := store.GetModel(t.Context(), id)
					require.NoError(t, err)
					base := Rates{Input: source.Input, Output: source.Output, CacheRead: source.CacheRead, CacheWrite: source.CacheWrite}
					assert.Equal(t, base, model.Cost.RatesFor(272_000))
					assert.Equal(t, source.Tiers[0].Rates, model.Cost.RatesFor(272_001))
					assert.Equal(t, source.Tiers[0].Rates, model.Cost.RatesFor(272_002))
					assert.Equal(t, original, *source, "shared catalogue must not be mutated")
				})
			}
		})
	}
}

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
