package config

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/paths"
)

func TestClonePreservesCatalogFallbackAndOverride(t *testing.T) {
	paths.SetCacheDir(t.TempDir())
	t.Cleanup(func() { paths.SetCacheDir("") })
	calls := 0
	fallback := func() *modelsdev.Database {
		calls++
		return &modelsdev.Database{Providers: map[string]modelsdev.Provider{
			"custom": {Models: map[string]modelsdev.Model{"model": {Name: "fallback"}}},
		}}
	}
	rc := &RuntimeConfig{
		EnvProviderOverride: environment.NewMapEnvProvider(nil),
		ModelsDevFallback:   fallback,
	}
	clone := rc.Clone()
	assert.Zero(t, calls, "cloning may construct the store, not load its catalog")
	actual, err := clone.ModelsDevStore()
	require.NoError(t, err)
	original, err := rc.ModelsDevStore()
	require.NoError(t, err)
	assert.Same(t, original, actual)
	ctx, cancel := context.WithDeadline(t.Context(), time.Unix(0, 0))
	defer cancel()
	model, err := actual.GetModel(ctx, modelsdev.NewID("custom", "model"))
	require.NoError(t, err)
	assert.Equal(t, "fallback", model.Name)
	assert.Equal(t, 1, calls)

	store := modelsdev.NewDatabaseStore(&modelsdev.Database{})
	rc.ModelsDevStoreOverride = store
	actual, err = rc.Clone().ModelsDevStore()
	require.NoError(t, err)
	assert.Same(t, store, actual)
	_, err = actual.GetModel(ctx, modelsdev.NewID("custom", "model"))
	require.Error(t, err)
	assert.Equal(t, 1, calls, "override must not use the configured fallback")
}
