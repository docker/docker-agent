package runtime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

// dmrRuntime builds a LocalRuntime whose DMR discovery is stubbed with the
// given lister, no models gateway, and no configured models unless provided.
func dmrRuntime(lister func(context.Context) ([]string, error), store ModelStore, models map[string]latest.ModelConfig) *LocalRuntime {
	return &LocalRuntime{
		modelsStore:    store,
		dmrModelLister: lister,
		now:            time.Now,
		modelSwitcherCfg: &ModelSwitcherConfig{
			EnvProvider: environment.NewNoEnvProvider(),
			Models:      models,
		},
	}
}

func refsOf(choices []ModelChoice) []string {
	out := make([]string, 0, len(choices))
	for _, c := range choices {
		out = append(out, c.Ref)
	}
	return out
}

func TestBuildDMRChoices(t *testing.T) {
	t.Parallel()

	t.Run("nil lister yields no DMR entries", func(t *testing.T) {
		t.Parallel()

		r := dmrRuntime(nil, nil, nil)
		assert.Empty(t, r.buildDMRChoices(t.Context()))
	})

	t.Run("discovery error yields no DMR entries", func(t *testing.T) {
		t.Parallel()

		r := dmrRuntime(func(context.Context) ([]string, error) {
			return nil, errors.New("dmr not installed")
		}, nil, nil)
		assert.Empty(t, r.buildDMRChoices(t.Context()))
	})

	t.Run("installed models become dmr-prefixed choices", func(t *testing.T) {
		t.Parallel()

		r := dmrRuntime(func(context.Context) ([]string, error) {
			return []string{"ai/qwen3:latest", "ai/gemma3:latest"}, nil
		}, nil, nil)

		choices := r.buildDMRChoices(t.Context())
		require.Len(t, choices, 2)

		byRef := map[string]ModelChoice{}
		for _, c := range choices {
			byRef[c.Ref] = c
		}

		qwen, ok := byRef["dmr/ai/qwen3:latest"]
		require.True(t, ok, "expected dmr/ai/qwen3:latest, got %v", refsOf(choices))
		assert.Equal(t, "dmr", qwen.Provider)
		assert.Equal(t, "ai/qwen3:latest", qwen.Model)
		assert.Equal(t, "ai/qwen3:latest", qwen.Name)
		// Discovered (not configured) DMR models group with catalog entries.
		assert.True(t, qwen.IsCatalog)

		// The ref round-trips back to provider="dmr" + the full model id.
		parsed, err := latest.ParseModelRef(qwen.Ref)
		require.NoError(t, err)
		assert.Equal(t, "dmr", parsed.Provider)
		assert.Equal(t, "ai/qwen3:latest", parsed.Model)
	})

	t.Run("embedding models are filtered out by name", func(t *testing.T) {
		t.Parallel()

		r := dmrRuntime(func(context.Context) ([]string, error) {
			return []string{"ai/qwen3:latest", "ai/embeddinggemma"}, nil
		}, nil, nil)

		assert.Equal(t, []string{"dmr/ai/qwen3:latest"}, refsOf(r.buildDMRChoices(t.Context())))
	})

	t.Run("embedding models are filtered out by catalog family even without an 'embed' id substring", func(t *testing.T) {
		t.Parallel()

		// The vector model's ID contains no "embed" substring; only the
		// models.dev Family marks it, exercising the metadata-before-filter
		// ordering in buildDMRChoices.
		store := stubModelStore{models: map[string]*modelsdev.Model{
			"dmr/ai/nomic-text-v1.5": {Name: "Nomic Text", Family: "text-embedding"},
		}}

		r := dmrRuntime(func(context.Context) ([]string, error) {
			return []string{"ai/qwen3:latest", "ai/nomic-text-v1.5"}, nil
		}, store, nil)

		assert.Equal(t, []string{"dmr/ai/qwen3:latest"}, refsOf(r.buildDMRChoices(t.Context())))
	})

	t.Run("models already in config are deduplicated", func(t *testing.T) {
		t.Parallel()

		r := dmrRuntime(func(context.Context) ([]string, error) {
			return []string{"ai/qwen3:latest", "ai/gemma3:latest"}, nil
		}, nil, map[string]latest.ModelConfig{
			"local": {Provider: "dmr", Model: "ai/qwen3:latest"},
		})

		assert.Equal(t, []string{"dmr/ai/gemma3:latest"}, refsOf(r.buildDMRChoices(t.Context())))
	})

	t.Run("catalog metadata is applied when available", func(t *testing.T) {
		t.Parallel()

		store := stubModelStore{models: map[string]*modelsdev.Model{
			"dmr/ai/qwen3:latest": {
				Name:   "Qwen 3",
				Family: "qwen",
				Limit:  modelsdev.Limit{Context: 32768},
			},
		}}

		r := dmrRuntime(func(context.Context) ([]string, error) {
			return []string{"ai/qwen3:latest"}, nil
		}, store, nil)

		choices := r.buildDMRChoices(t.Context())
		require.Len(t, choices, 1)
		assert.Equal(t, "Qwen 3", choices[0].Name)
		assert.Equal(t, "qwen", choices[0].Family)
		assert.Equal(t, 32768, choices[0].ContextLimit)
	})
}

func TestListDMRModelsCachesResult(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	r := dmrRuntime(func(context.Context) ([]string, error) {
		calls.Add(1)
		return []string{"ai/qwen3:latest"}, nil
	}, nil, nil)

	for range 3 {
		ids, err := r.listDMRModels(t.Context())
		require.NoError(t, err)
		assert.Equal(t, []string{"ai/qwen3:latest"}, ids)
	}

	assert.Equal(t, int32(1), calls.Load(), "lister should be called once within the TTL window")
}

func TestListDMRModels_CachesFailure(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	r := dmrRuntime(func(context.Context) ([]string, error) {
		calls.Add(1)
		return nil, errors.New("dmr unreachable")
	}, nil, nil)

	_, err := r.listDMRModels(t.Context())
	require.Error(t, err)
	_, err = r.listDMRModels(t.Context())
	require.Error(t, err)

	assert.Equal(t, int32(1), calls.Load(), "failures must be cached to avoid re-probing DMR on every picker open")
}

func TestListDMRModels_CacheExpires(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	r := dmrRuntime(func(context.Context) ([]string, error) {
		calls.Add(1)
		return []string{"ai/qwen3:latest"}, nil
	}, nil, nil)

	now := time.Now()
	r.now = func() time.Time { return now }

	_, err := r.listDMRModels(t.Context())
	require.NoError(t, err)

	now = now.Add(dmrModelsTTL + time.Second)
	_, err = r.listDMRModels(t.Context())
	require.NoError(t, err)

	assert.Equal(t, int32(2), calls.Load(), "DMR must be re-queried after the cache TTL")
}

func TestAvailableModelsIncludesDMR(t *testing.T) {
	t.Parallel()

	// A stub store with no database makes buildCatalogChoices a no-op, so the
	// only entries come from DMR discovery.
	r := dmrRuntime(func(context.Context) ([]string, error) {
		return []string{"ai/qwen3:latest"}, nil
	}, stubModelStore{}, nil)

	got := refsOf(r.AvailableModels(t.Context()))
	assert.Contains(t, got, "dmr/ai/qwen3:latest")
}
