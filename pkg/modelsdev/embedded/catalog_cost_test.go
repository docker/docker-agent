package embedded

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/modelsdev"
)

func TestVercelAstraContextBoundary(t *testing.T) {
	t.Parallel()

	db := Snapshot()
	for _, modelID := range []string{"openai/gpt-6-astra", "openai/gpt-6-astra-fast"} {
		t.Run(modelID, func(t *testing.T) {
			t.Parallel()
			id := modelsdev.NewID("vercel", modelID)
			source := db.Providers[id.Provider].Models[id.Model].Cost
			require.NotNil(t, source)
			original := *source
			original.Tiers = slices.Clone(source.Tiers)
			for _, path := range []string{"embedded", "cache", "fetch"} {
				t.Run(path, func(t *testing.T) {
					t.Parallel()
					store := modelsdev.NewDatabaseStore(db)
					if path != "embedded" {
						cache := filepath.Join(t.TempDir(), "models.json")
						if path == "cache" {
							writeCache(t, cache, *db)
						}
						var err error
						store, err = modelsdev.NewStore(modelsdev.WithCache(cache), modelsdev.WithFetcher(func(context.Context, string) (*modelsdev.Database, string, error) {
							assert.Equal(t, "fetch", path, "fresh cache must not fetch")
							return db, "etag", nil
						}))
						require.NoError(t, err)
					}
					model, err := store.GetModel(t.Context(), id)
					require.NoError(t, err)
					base := modelsdev.Rates{Input: source.Input, Output: source.Output, CacheRead: source.CacheRead, CacheWrite: source.CacheWrite}
					assert.Equal(t, base, model.Cost.RatesFor(272_000))
					assert.Equal(t, source.Tiers[0].Rates, model.Cost.RatesFor(272_001))
					assert.Equal(t, source.Tiers[0].Rates, model.Cost.RatesFor(272_002))
					assert.Equal(t, original, *source, "shared catalogue must not be mutated")
				})
			}
		})
	}
}

func writeCache(tb testing.TB, path string, db modelsdev.Database) {
	tb.Helper()
	data, err := json.Marshal(modelsdev.CachedData{Database: db, LastRefresh: time.Now()})
	require.NoError(tb, err)
	require.NoError(tb, os.WriteFile(path, data, 0o600))
}
