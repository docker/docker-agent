package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/runtime/modelcatalog"
	"github.com/docker/docker-agent/pkg/team"
)

func TestModelsDevFallbackIsLazyAndRespectsStorePrecedence(t *testing.T) {
	paths.SetCacheDir(t.TempDir())
	t.Cleanup(func() { paths.SetCacheDir("") })
	tm := team.New(team.WithAgents(agent.New("root", "test", agent.WithModel(&mockProvider{id: "test/model"}))))
	calls := 0
	database := &modelsdev.Database{Providers: map[string]modelsdev.Provider{
		"custom": {Models: map[string]modelsdev.Model{"model": {Name: "fallback", Limit: modelsdev.Limit{Context: 12345}}}},
	}}
	fallback := func() *modelsdev.Database { calls++; return database }
	rt, err := NewLocalRuntime(t.Context(), tm, WithModelsDevFallback(fallback))
	require.NoError(t, err)
	assert.Zero(t, calls)
	ctx, cancel := context.WithDeadline(t.Context(), time.Unix(0, 0))
	defer cancel()
	model, err := rt.modelsStore.GetModel(ctx, modelsdev.NewID("custom", "model"))
	require.NoError(t, err)
	assert.Equal(t, "fallback", model.Name)
	assert.Equal(t, 1, calls)

	store := modelsdev.NewDatabaseStore(&modelsdev.Database{})
	for _, option := range []Opt{WithModelStore(store), WithModelSwitcherConfig(&modelcatalog.Config{ModelsStore: store})} {
		rt, err := NewLocalRuntime(t.Context(), tm, WithModelsDevFallback(fallback), option)
		require.NoError(t, err)
		assert.Same(t, store, rt.modelsStore)
		_, err = rt.modelsStore.GetModel(ctx, modelsdev.NewID("custom", "model"))
		require.Error(t, err)
	}
	assert.Equal(t, 1, calls)
}
