package modelsdev

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFallbackIsOptionalAndLazy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		fallback func() *Database
	}{
		{"unset", nil},
		{"nil result", func() *Database { return nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, fetch := trackingFetcher()
			store, err := NewStore(WithCache(filepath.Join(t.TempDir(), "catalog.json")), WithFetcher(fetch), WithFallback(tc.fallback))
			require.NoError(t, err)
			db, err := store.GetDatabase(t.Context())
			require.NoError(t, err)
			require.NotNil(t, db)
			assert.Empty(t, db.Providers)
		})
	}
}

func TestFallbackSelectionAndRecovery(t *testing.T) {
	t.Parallel()
	fallback := &Database{Providers: map[string]Provider{"local": {Models: map[string]Model{"model": {Name: "fallback"}}}}}
	fresh := &Database{Providers: map[string]Provider{"fresh": {}}}
	calls, fetches := 0, 0
	store, err := NewStore(
		WithCache(filepath.Join(t.TempDir(), "catalog.json")),
		WithFallback(func() *Database { calls++; return fallback }),
		WithFetcher(func(context.Context, string) (*Database, string, error) {
			fetches++
			if fetches == 1 {
				return nil, "", errors.New("offline")
			}
			return fresh, "etag", nil
		}),
	)
	require.NoError(t, err)
	assert.Zero(t, calls, "construction must not evaluate the fallback")
	db, err := store.GetDatabase(t.Context())
	require.NoError(t, err)
	assert.Same(t, fallback, db)
	assert.Equal(t, 1, calls)
	db, err = store.GetDatabase(t.Context())
	require.NoError(t, err)
	assert.Same(t, fresh, db, "a fallback must not hide network recovery")
	assert.Equal(t, 1, calls)
	assert.Equal(t, 2, fetches)
}

func TestCacheAndCustomProviderFallback(t *testing.T) {
	t.Parallel()
	fallback := &Database{Providers: map[string]Provider{"custom": {Models: map[string]Model{"model": {Name: "custom"}}}}}
	calls := 0
	path := filepath.Join(t.TempDir(), "catalog.json")
	fetched, fetch := trackingFetcher()
	store, err := NewStore(WithCache(path), WithFetcher(fetch), WithKnownProvider(func(string) bool { return false }), WithFallback(func() *Database { calls++; return fallback }))
	require.NoError(t, err)
	model, err := store.GetModel(t.Context(), NewID("custom", "model"))
	require.NoError(t, err)
	assert.Equal(t, "custom", model.Name)
	assert.False(t, *fetched)
	assert.Equal(t, 1, calls)

	writeCache(t, path, Database{Providers: map[string]Provider{"cached": {}}})
	cached, err := NewStore(WithCache(path), WithFetcher(fetch), WithFallback(func() *Database { t.Error("fresh cache must win"); return fallback }))
	require.NoError(t, err)
	db, err := cached.GetDatabase(t.Context())
	require.NoError(t, err)
	assert.Contains(t, db.Providers, "cached")
}
