package modelsdev

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeCache writes a fresh on-disk catalog cache the Store can load without
// touching the network.
func writeCache(tb testing.TB, path string, db Database) {
	tb.Helper()
	data, err := json.Marshal(CachedData{Database: db, LastRefresh: time.Now()})
	require.NoError(tb, err)
	require.NoError(tb, os.WriteFile(path, data, 0o600))
}

// expiredContext returns a context whose deadline is already in the past, so
// any HTTP request built from it fails instantly without touching the network.
// It stands in for an environment where models.dev is unreachable.
func expiredContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithDeadline(t.Context(), time.Unix(0, 0))
	t.Cleanup(cancel)
	return ctx
}

// TestKnownProviderGatesFetch is the regression test for issue #3165: a lookup
// for a provider the knownProvider predicate rejects (a user-defined custom
// provider) must resolve locally without ever fetching the models.dev catalog,
// while a known provider may still trigger a fetch.
func TestKnownProviderGatesFetch(t *testing.T) {
	t.Parallel()

	// A cache path that does not exist, so there is no on-disk catalog to fall
	// back on — the cold-start situation from the issue.
	cacheFile := filepath.Join(t.TempDir(), "models_dev.json")
	store, err := NewStore(
		WithCache(cacheFile),
		WithKnownProvider(func(p string) bool { return p == "openai" }),
	)
	require.NoError(t, err)

	ctx := expiredContext(t)

	// Custom provider: not known -> resolves locally, no network attempt.
	_, err = store.GetModel(ctx, NewID("mistral_gateway", "mistral-small-latest"))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "fetch from API",
		"custom provider must not trigger a models.dev fetch")
	assert.Contains(t, err.Error(), `provider "mistral_gateway" not found`)

	// Known provider: a cold cache still warrants a fetch, which fails here
	// only because the network is unreachable — confirming the gate did not
	// disable fetching wholesale.
	_, err = store.GetModel(ctx, NewID("openai", "gpt-4o"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fetch from API",
		"known provider must still fetch the catalog when the cache is cold")
}

// TestNoPredicateAlwaysAllowsFetch guards the default, backwards-compatible
// behaviour: with no knownProvider predicate every provider may fetch.
func TestNoPredicateAlwaysAllowsFetch(t *testing.T) {
	t.Parallel()

	cacheFile := filepath.Join(t.TempDir(), "models_dev.json")
	store, err := NewStore(WithCache(cacheFile))
	require.NoError(t, err)

	_, err = store.GetModel(expiredContext(t), NewID("mistral_gateway", "mistral-small-latest"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fetch from API")
}

// TestFetchDisallowedServesFromCache checks the cache-only path: a provider the
// predicate rejects but that IS present in the on-disk cache resolves from the
// cache without any network call, and an absent one is a clean "not found".
func TestFetchDisallowedServesFromCache(t *testing.T) {
	t.Parallel()

	cacheFile := filepath.Join(t.TempDir(), "models_dev.json")
	writeCache(t, cacheFile, Database{Providers: map[string]Provider{
		// Present in the catalog but not in the known-provider set below.
		"deepseek": {Models: map[string]Model{
			"deepseek-chat": {Name: "DeepSeek Chat", Limit: Limit{Context: 64000}},
		}},
	}})

	store, err := NewStore(
		WithCache(cacheFile),
		WithKnownProvider(func(p string) bool { return p == "openai" }),
	)
	require.NoError(t, err)

	// Expired context: proves resolution is cache-only (no network) for a
	// fetch-disallowed provider.
	ctx := expiredContext(t)

	m, err := store.GetModel(ctx, NewID("deepseek", "deepseek-chat"))
	require.NoError(t, err)
	assert.Equal(t, 64000, m.Limit.Context)

	// Repeated lookups stay consistent (served from the memoized snapshot).
	again, err := store.GetModel(ctx, NewID("deepseek", "deepseek-chat"))
	require.NoError(t, err)
	assert.Equal(t, m.Limit.Context, again.Limit.Context)

	// A provider absent from the cache is still a clean not-found, no fetch.
	_, err = store.GetModel(ctx, NewID("mistral_gateway", "x"))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "fetch from API")
}

// BenchmarkGetModelFetchDisallowed guards the hot path: repeatedly resolving a
// fetch-disallowed provider against a warm, sizable cache must not re-read and
// re-parse the catalog file each call (the snapshot is memoized).
func BenchmarkGetModelFetchDisallowed(b *testing.B) {
	cacheFile := filepath.Join(b.TempDir(), "models_dev.json")
	providers := make(map[string]Provider, 200)
	for i := range 200 {
		models := make(map[string]Model, 50)
		for j := range 50 {
			id := fmt.Sprintf("m-%d-%d", i, j)
			models[id] = Model{Name: id, Limit: Limit{Context: 128000}}
		}
		providers[fmt.Sprintf("prov-%d", i)] = Provider{Models: models}
	}
	writeCache(b, cacheFile, Database{Providers: providers})

	store, err := NewStore(
		WithCache(cacheFile),
		WithKnownProvider(func(p string) bool { return p == "openai" }),
	)
	require.NoError(b, err)

	id := NewID("mistral_gateway", "whatever")
	ctx := b.Context()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, _ = store.GetModel(ctx, id)
	}
}

func TestResolveModelAlias(t *testing.T) {
	t.Parallel()

	mockData := &Database{
		Providers: map[string]Provider{
			"anthropic": {
				Models: map[string]Model{
					// Pattern 1: alias has same prefix as pinned
					"claude-sonnet-4-5":          {Name: "Claude Sonnet 4.5 (latest)"},
					"claude-sonnet-4-5-20250929": {Name: "Claude Sonnet 4.5"},
					// Pattern 2: alias ends with -0 which gets dropped
					"claude-sonnet-4-0":        {Name: "Claude Sonnet 4 (latest)"},
					"claude-sonnet-4-20250514": {Name: "Claude Sonnet 4"},
					// Pattern 3: -latest suffix style
					"claude-3-5-sonnet-latest":   {Name: "Claude 3.5 Sonnet (latest)"},
					"claude-3-5-sonnet-20241022": {Name: "Claude 3.5 Sonnet"},
					// A pinned model without an alias
					"claude-3-opus-20240229": {Name: "Claude 3 Opus"},
				},
			},
			"openai": {
				Models: map[string]Model{
					"gpt-4o":            {Name: "GPT-4o (latest)"},
					"gpt-4o-2024-11-20": {Name: "GPT-4o"},
				},
			},
		},
	}

	store := NewDatabaseStore(mockData)

	tests := []struct {
		name     string
		provider string
		model    string
		expected string
	}{
		{"resolves alias with same prefix", "anthropic", "claude-sonnet-4-5", "claude-sonnet-4-5-20250929"},
		{"resolves alias with -0 suffix", "anthropic", "claude-sonnet-4-0", "claude-sonnet-4-20250514"},
		{"resolves alias with -latest suffix", "anthropic", "claude-3-5-sonnet-latest", "claude-3-5-sonnet-20241022"},
		{"keeps pinned model unchanged", "anthropic", "claude-sonnet-4-5-20250929", "claude-sonnet-4-5-20250929"},
		{"keeps pinned model without alias unchanged", "anthropic", "claude-3-opus-20240229", "claude-3-opus-20240229"},
		{"resolves openai alias", "openai", "gpt-4o", "gpt-4o-2024-11-20"},
		{"returns original for unknown provider", "unknown", "model", "model"},
		{"returns original for unknown model", "anthropic", "unknown-model", "unknown-model"},
		{"returns original for empty provider", "", "model", "model"},
		{"returns original for empty model", "anthropic", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := store.ResolveModelAlias(t.Context(), tt.provider, tt.model)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestDatePattern(t *testing.T) {
	t.Parallel()

	tests := []struct {
		modelID string
		matches bool
	}{
		{"claude-sonnet-4-5-20250929", true},
		{"gpt-4o-2024-11-20", true},
		{"claude-3-opus-20240229", true},
		{"claude-sonnet-4-5", false},
		{"gpt-4o", false},
		{"some-model-123", false},
	}

	for _, tt := range tests {
		t.Run(tt.modelID, func(t *testing.T) {
			assert.Equal(t, tt.matches, datePattern.MatchString(tt.modelID))
		})
	}
}
