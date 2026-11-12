package modelsdev

import (
	"context"
	"encoding/json"
	"errors"
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

// trackingFetcher returns a stub fetcher that records whether a fetch was
// attempted and always reports the network as unreachable, along with a
// pointer to the "fetched" flag. Injected via WithFetcher so tests never
// mutate package-level state and stay safe to run in parallel.
func trackingFetcher() (fetched *bool, fetch fetcher) {
	fetched = new(bool)
	return fetched, func(context.Context, string) (*Database, string, error) {
		*fetched = true
		return nil, "", errors.New("fetch from API: network unreachable")
	}
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
	fetched, fetch := trackingFetcher()
	store, err := NewStore(
		WithCache(cacheFile),
		WithKnownProvider(func(p string) bool { return p == "openai" }),
		WithFetcher(fetch),
	)
	require.NoError(t, err)

	ctx := expiredContext(t)

	// Custom provider: not known -> resolves locally, no network attempt.
	*fetched = false
	_, err = store.GetModel(ctx, NewID("mistral_gateway", "mistral-small-latest"))
	require.Error(t, err)
	assert.False(t, *fetched, "custom provider must not trigger a models.dev fetch")
	assert.Contains(t, err.Error(), `provider "mistral_gateway" not found`)

	// Known provider: a cold cache still warrants a fetch — confirming the gate
	// did not disable fetching wholesale. The fetch fails here (unreachable
	// network) so the lookup falls back to the embedded snapshot.
	*fetched = false
	_, _ = store.GetModel(ctx, NewID("openai", "gpt-4o"))
	assert.True(t, *fetched, "known provider must still fetch the catalog when the cache is cold")
}

// TestNoPredicateAlwaysAllowsFetch guards the default, backwards-compatible
// behaviour: with no knownProvider predicate every provider may fetch.
func TestNoPredicateAlwaysAllowsFetch(t *testing.T) {
	t.Parallel()

	cacheFile := filepath.Join(t.TempDir(), "models_dev.json")
	fetched, fetch := trackingFetcher()
	store, err := NewStore(WithCache(cacheFile), WithFetcher(fetch))
	require.NoError(t, err)

	_, _ = store.GetModel(expiredContext(t), NewID("mistral_gateway", "mistral-small-latest"))
	assert.True(t, *fetched, "with no predicate, any provider may trigger a fetch")
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

// TestRefreshBypassesFreshCache checks that Refresh forces a fetch even when
// the on-disk cache is fresh (GetDatabase would not have fetched), replaces
// the memoized catalog, and rewrites the cache file.
func TestRefreshBypassesFreshCache(t *testing.T) {
	t.Parallel()

	cacheFile := filepath.Join(t.TempDir(), "models_dev.json")
	writeCache(t, cacheFile, Database{Providers: map[string]Provider{
		"openai": {Models: map[string]Model{"gpt-old": {Name: "GPT Old"}}},
	}})

	fetched := false
	store, err := NewStore(WithCache(cacheFile), WithFetcher(func(context.Context, string) (*Database, string, error) {
		fetched = true
		return &Database{Providers: map[string]Provider{
			"openai": {Models: map[string]Model{"gpt-new": {Name: "GPT New"}}},
		}}, `"etag-2"`, nil
	}))
	require.NoError(t, err)

	// Warm the store from the fresh cache: no fetch happens.
	_, err = store.GetModel(t.Context(), NewID("openai", "gpt-old"))
	require.NoError(t, err)
	assert.False(t, fetched, "a fresh cache must not trigger a fetch")

	require.NoError(t, store.Refresh(t.Context()))
	assert.True(t, fetched, "Refresh must fetch even when the cache is fresh")

	// The memoized catalog now serves the refreshed data.
	_, err = store.GetModel(t.Context(), NewID("openai", "gpt-new"))
	require.NoError(t, err)
	_, err = store.GetModel(t.Context(), NewID("openai", "gpt-old"))
	require.Error(t, err)

	// And the on-disk cache was rewritten with the fresh catalog and ETag.
	cached, err := loadFromCache(cacheFile)
	require.NoError(t, err)
	assert.Contains(t, cached.Database.Providers["openai"].Models, "gpt-new")
	assert.Equal(t, `"etag-2"`, cached.ETag)
}

// TestRefreshNotModifiedKeepsCache checks the 304 path: an unchanged catalog
// keeps the cached data and only bumps its LastRefresh timestamp.
func TestRefreshNotModifiedKeepsCache(t *testing.T) {
	t.Parallel()

	cacheFile := filepath.Join(t.TempDir(), "models_dev.json")
	data, err := json.Marshal(CachedData{
		Database: Database{Providers: map[string]Provider{
			"openai": {Models: map[string]Model{"gpt-4o": {Name: "GPT-4o"}}},
		}},
		LastRefresh: time.Now().Add(-48 * time.Hour),
		ETag:        `"etag-1"`,
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(cacheFile, data, 0o600))

	store, err := NewStore(WithCache(cacheFile), WithFetcher(func(_ context.Context, etag string) (*Database, string, error) {
		assert.Equal(t, `"etag-1"`, etag, "Refresh must send the cached ETag")
		return nil, etag, nil // 304 Not Modified
	}))
	require.NoError(t, err)

	require.NoError(t, store.Refresh(t.Context()))

	m, err := store.GetModel(t.Context(), NewID("openai", "gpt-4o"))
	require.NoError(t, err)
	assert.Equal(t, "GPT-4o", m.Name)

	cached, err := loadFromCache(cacheFile)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now(), cached.LastRefresh, time.Minute, "304 must bump LastRefresh")
}

// TestRefreshFetchError leaves the store usable on a failed refresh.
func TestRefreshFetchError(t *testing.T) {
	t.Parallel()

	cacheFile := filepath.Join(t.TempDir(), "models_dev.json")
	writeCache(t, cacheFile, Database{Providers: map[string]Provider{
		"openai": {Models: map[string]Model{"gpt-4o": {Name: "GPT-4o"}}},
	}})

	_, fetch := trackingFetcher()
	store, err := NewStore(WithCache(cacheFile), WithFetcher(fetch))
	require.NoError(t, err)

	require.Error(t, store.Refresh(t.Context()))

	// The cached catalog still serves lookups.
	_, err = store.GetModel(t.Context(), NewID("openai", "gpt-4o"))
	require.NoError(t, err)
}

func TestRefreshInMemoryStoreReturnsError(t *testing.T) {
	t.Parallel()

	store := NewDatabaseStore(&Database{})
	assert.Error(t, store.Refresh(t.Context()))
}

func TestRefreshNotModifiedWithoutCacheReturnsError(t *testing.T) {
	t.Parallel()

	store, err := NewStore(
		WithCache(filepath.Join(t.TempDir(), "models_dev.json")),
		WithFetcher(func(context.Context, string) (*Database, string, error) {
			return nil, "", nil
		}),
	)
	require.NoError(t, err)
	assert.Error(t, store.Refresh(t.Context()))
}

func TestRefreshDoesNotBlockReadersDuringFetch(t *testing.T) {
	t.Parallel()

	cacheFile := filepath.Join(t.TempDir(), "models_dev.json")
	writeCache(t, cacheFile, Database{Providers: map[string]Provider{
		"openai": {Models: map[string]Model{"gpt-4o": {Name: "GPT-4o"}}},
	}})

	started := make(chan struct{})
	release := make(chan struct{})
	store, err := NewStore(WithCache(cacheFile), WithFetcher(func(context.Context, string) (*Database, string, error) {
		close(started)
		<-release
		return &Database{}, "", nil
	}))
	require.NoError(t, err)

	// Warm the authoritative in-memory database before refreshing.
	_, err = store.GetDatabase(t.Context())
	require.NoError(t, err)

	refreshDone := make(chan error, 1)
	go func() { refreshDone <- store.Refresh(t.Context()) }()
	<-started

	readDone := make(chan struct{})
	go func() {
		_, _ = store.GetDatabase(t.Context())
		close(readDone)
	}()
	select {
	case <-readDone:
	case <-time.After(time.Second):
		t.Fatal("GetDatabase blocked behind the refresh network request")
	}

	close(release)
	require.NoError(t, <-refreshDone)
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

func TestStore_GetModel_CatalogProviderAliases(t *testing.T) {
	t.Parallel()

	catalogModel := Model{
		Name: "Vision model", Family: "vision", Reasoning: true, ToolCall: true,
		Temperature: true, Attachment: true, OpenWeights: true, ReleaseDate: "2026-09-01",
		Cost: &Cost{
			Input: 1.2, Output: 3.4, CacheRead: 0.1, CacheWrite: 0.2,
			Tiers: []CostTier{{Rates: Rates{Input: 2.4, Output: 6.8}, Tier: TierSpec{Type: "context", Size: 100000}}},
		},
		Limit:      Limit{Context: 262144, Output: 65536},
		Modalities: Modalities{Input: []string{"text", "image"}, Output: []string{"text"}},
	}
	directModel := Model{Name: "Direct entry", Limit: Limit{Context: 1000, Output: 500}}
	for _, alias := range []struct{ configured, catalog string }{
		{"fireworks", "fireworks-ai"},
		{"together", "togetherai"},
		{"moonshot", "moonshotai"},
		{"opencode-zen", "opencode"},
	} {
		for _, mode := range []string{"catalog only", "direct entry wins", "direct provider missing model"} {
			t.Run(alias.configured+"/"+mode, func(t *testing.T) {
				t.Parallel()
				providers := map[string]Provider{alias.catalog: {Models: map[string]Model{"MixedModel": catalogModel}}}
				want := catalogModel
				switch mode {
				case "direct entry wins":
					providers[alias.configured] = Provider{Models: map[string]Model{"MixedModel": directModel}}
					want = directModel
				case "direct provider missing model":
					providers[alias.configured] = Provider{Models: map[string]Model{"other": directModel}}
				}
				store := NewDatabaseStore(&Database{Providers: providers})
				got, err := store.GetModel(t.Context(), NewID(alias.configured, "MixedModel"))
				require.NoError(t, err)
				assert.Equal(t, &want, got)
				canonicalModel, err := store.GetModel(t.Context(), NewID(alias.catalog, "MixedModel"))
				require.NoError(t, err)
				assert.Equal(t, &catalogModel, canonicalModel)
				_, err = store.GetModel(t.Context(), NewID(alias.configured, "missing"))
				require.EqualError(t, err, `model "missing" not found in provider "`+alias.configured+`"`)
				_, err = store.GetModel(t.Context(), NewID(alias.configured, "mixedmodel"))
				require.EqualError(t, err, `model "mixedmodel" not found in provider "`+alias.configured+`"`)
			})
		}
	}
}

func TestStore_GetModel_CatalogLookupBoundaries(t *testing.T) {
	t.Parallel()

	lower := Model{Name: "Lowercase catalog model", Modalities: Modalities{Input: []string{"text", "image"}}}
	direct := Model{Name: "Exact catalog model"}
	for _, tc := range []struct {
		name      string
		id        ID
		providers map[string]Provider
		want      *Model
		wantErr   string
	}{
		{
			name: "OVH lowercase fallback", id: NewID("ovhcloud", "Qwen3.5-397B-A17B"),
			providers: map[string]Provider{"ovhcloud": {Models: map[string]Model{"qwen3.5-397b-a17b": lower}}}, want: &lower,
		},
		{
			name: "OVH direct precedence", id: NewID("ovhcloud", "Qwen3.5-397B-A17B"),
			providers: map[string]Provider{"ovhcloud": {Models: map[string]Model{"qwen3.5-397b-a17b": lower, "Qwen3.5-397B-A17B": direct}}}, want: &direct,
		},
		{
			name: "OVH unknown preserves spelling", id: NewID("ovhcloud", "MissingModel"),
			providers: map[string]Provider{"ovhcloud": {}}, wantErr: `model "MissingModel" not found in provider "ovhcloud"`,
		},
		{
			name: "other providers case sensitive", id: NewID("openai", "MixedModel"),
			providers: map[string]Provider{"openai": {Models: map[string]Model{"mixedmodel": lower}}}, wantErr: `model "MixedModel" not found in provider "openai"`,
		},
		{
			name: "ChatGPT is not a full OpenAI alias", id: NewID("chatgpt", "vision"),
			providers: map[string]Provider{"openai": {Models: map[string]Model{"vision": lower}}}, wantErr: `provider "chatgpt" not found`,
		},
		{
			name: "alias missing provider", id: NewID("fireworks", "MissingModel"),
			providers: map[string]Provider{}, wantErr: `provider "fireworks" not found`,
		},
		{
			name: "custom provider missing", id: NewID("custom", "MissingModel"),
			providers: map[string]Provider{}, wantErr: `provider "custom" not found`,
		},
		{
			name: "Bedrock direct precedence", id: NewID("amazon-bedrock", "us.model"),
			providers: map[string]Provider{"amazon-bedrock": {Models: map[string]Model{"model": lower, "us.model": direct}}}, want: &direct,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := NewDatabaseStore(&Database{Providers: tc.providers}).GetModel(t.Context(), tc.id)
			if tc.wantErr != "" {
				require.EqualError(t, err, tc.wantErr)
				assert.Nil(t, got)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.want, got)
			}
		})
	}
}

func TestStore_GetModel_CatalogAliasFetchPolicy(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		known     bool
		fail      bool
		cancel    bool
		missing   bool
		wantFetch int
	}{
		{name: "known success", known: true, wantFetch: 1},
		{name: "known missing model", known: true, missing: true, wantFetch: 1},
		{name: "known fetch failure", known: true, fail: true, wantFetch: 1},
		{name: "known canceled fetch", known: true, cancel: true, wantFetch: 1},
		{name: "custom never fetches"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls int
			var checked []string
			model := "catalog-fetch-test"
			store, err := NewStore(
				WithCache(filepath.Join(t.TempDir(), CacheFileName)),
				WithKnownProvider(func(provider string) bool {
					checked = append(checked, provider)
					return tc.known && provider == "moonshot"
				}),
				WithFetcher(func(ctx context.Context, _ string) (*Database, string, error) {
					calls++
					if tc.cancel {
						require.ErrorIs(t, ctx.Err(), context.Canceled)
						return nil, "", ctx.Err()
					}
					if tc.fail {
						return nil, "", errors.New("offline")
					}
					models := map[string]Model{}
					if !tc.missing {
						models[model] = Model{Name: "Fetched model"}
					}
					return &Database{Providers: map[string]Provider{"moonshotai": {Models: models}}}, "", nil
				}),
			)
			require.NoError(t, err)
			ctx := t.Context()
			if tc.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			provider := "moonshot"
			if !tc.known {
				provider = "custom"
			}
			got, err := store.GetModel(ctx, NewID(provider, model))
			if tc.known && !tc.fail && !tc.cancel && !tc.missing {
				require.NoError(t, err)
				assert.Equal(t, &Model{Name: "Fetched model"}, got)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), provider)
				assert.Nil(t, got)
			}
			assert.Equal(t, tc.wantFetch, calls)
			assert.Equal(t, []string{provider}, checked, "fetch policy uses only the configured provider")
		})
	}
}

func TestCanonicalProviderID(t *testing.T) {
	t.Parallel()
	for legacy, canonical := range legacyProviderIDs {
		assert.Equal(t, canonical, CanonicalProviderID(legacy))
		assert.Equal(t, canonical, CanonicalProviderID(canonical))
	}
	for _, id := range []string{"", "chatgpt", "openai", "custom", "FIREWORKS"} {
		assert.Equal(t, id, CanonicalProviderID(id))
	}
}

func TestStore_ResolveModelAlias_LegacyProviders(t *testing.T) {
	t.Parallel()
	for legacy, canonical := range legacyProviderIDs {
		t.Run(legacy, func(t *testing.T) {
			t.Parallel()
			catalog := Provider{Models: map[string]Model{
				"latest":         {Name: "Model (latest)"},
				"model-20260101": {Name: "Model"},
			}}
			db := &Database{Providers: map[string]Provider{canonical: catalog}}
			store := NewDatabaseStore(db)
			assert.Equal(t, "model-20260101", store.ResolveModelAlias(t.Context(), legacy, "latest"))
			assert.Equal(t, "model-20260101", store.ResolveModelAlias(t.Context(), canonical, "latest"))
			assert.Equal(t, "unknown", store.ResolveModelAlias(t.Context(), legacy, "unknown"))
			db.Providers[legacy] = Provider{Models: map[string]Model{"latest": {Name: "Direct model"}}}
			assert.Equal(t, "latest", store.ResolveModelAlias(t.Context(), legacy, "latest"), "direct provider wins")
			_, err := store.GetModel(t.Context(), NewID(canonical, "latest"))
			require.NoError(t, err)
		})
	}
}
