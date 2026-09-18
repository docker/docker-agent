package runtime

import (
	"context"
	"errors"
	"math"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/dmr/dmrmodels"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

// dmrRuntime builds a LocalRuntime whose DMR discovery is stubbed with the
// given lister, no models gateway, and no configured models unless provided.
func dmrRuntime(lister func(context.Context) ([]string, error), store ModelStore, models map[string]latest.ModelConfig) *LocalRuntime {
	var details func(context.Context) ([]dmrmodels.Model, error)
	if lister != nil {
		details = func(ctx context.Context) ([]dmrmodels.Model, error) {
			ids, err := lister(ctx)
			models := make([]dmrmodels.Model, len(ids))
			for i, id := range ids {
				models[i].ID = id
			}
			return models, err
		}
	}
	return &LocalRuntime{
		modelsStore:    store,
		dmrModelLister: details,
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
		assert.Equal(t, []dmrmodels.Model{{ID: "ai/qwen3:latest"}}, ids)
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

func TestListDMRModels_DoesNotCacheCallerCancellation(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	r := dmrRuntime(func(ctx context.Context) ([]string, error) {
		calls.Add(1)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return []string{"ai/qwen3:latest"}, nil
	}, nil, nil)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := r.listDMRModels(ctx)
	require.Error(t, err)

	ids, err := r.listDMRModels(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []dmrmodels.Model{{ID: "ai/qwen3:latest"}}, ids)
	assert.Equal(t, int32(2), calls.Load(), "caller cancellation must not poison the DMR discovery cache")
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

func TestDMRMetadataInPicker(t *testing.T) {
	t.Parallel()
	r := dmrRuntime(nil, stubModelStore{}, map[string]latest.ModelConfig{
		"local":  {Provider: "dmr", Model: "ai/qwen3", ProviderOpts: map[string]any{"context_size": 8192}},
		"remote": {Provider: "dmr", Model: "ai/gemma3", BaseURL: "http://remote/engines/v1", ProviderOpts: map[string]any{"context_size": 4096}},
	})
	calls := 0
	r.dmrModelLister = func(context.Context) ([]dmrmodels.Model, error) {
		calls++
		return []dmrmodels.Model{
			{ID: "ai/qwen3:latest", Metadata: &dmrmodels.Metadata{Architecture: "qwen3", ContextWindow: 32768, Parameters: "8B", Quantization: "Q4_K_M", Size: "4.9 GiB"}},
			{ID: "ai/gemma3", Metadata: &dmrmodels.Metadata{ContextWindow: 131072}},
		}, nil
	}
	choices := r.AvailableModels(t.Context())
	byRef := map[string]ModelChoice{}
	for _, choice := range choices {
		byRef[choice.Ref] = choice
	}
	assert.Equal(t, 8192, byRef["local"].ContextLimit)
	assert.Equal(t, "qwen3", byRef["local"].Architecture)
	assert.Equal(t, "8B", byRef["local"].Parameters)
	assert.Equal(t, "Q4_K_M", byRef["local"].Quantization)
	assert.Equal(t, "4.9 GiB", byRef["local"].Size)
	assert.Equal(t, 4096, byRef["remote"].ContextLimit, "must use explicit context, not local metadata for a remote model")
	assert.Equal(t, 32768, byRef["dmr/ai/qwen3:latest"].ContextLimit)
	assert.Equal(t, 1, calls, "configured and discovered choices share one request")
}

func TestConfiguredContextWithoutDiscovery(t *testing.T) {
	t.Parallel()
	r := dmrRuntime(func(context.Context) ([]string, error) { return nil, errors.New("unavailable") }, stubModelStore{}, map[string]latest.ModelConfig{
		"local": {Provider: "dmr", Model: "ai/qwen3", ProviderOpts: map[string]any{"context_size": 4096}},
	})
	choices := r.AvailableModels(t.Context())
	require.Len(t, choices, 1)
	assert.Equal(t, 4096, choices[0].ContextLimit)
}

func TestConfiguredContextIntegerBounds(t *testing.T) {
	t.Parallel()

	for _, n := range []int64{32768, math.MaxInt32, math.MaxInt32 + 1, math.MaxInt64} {
		value := strconv.FormatInt(n, 10)
		t.Run(value, func(t *testing.T) {
			t.Parallel()

			for _, discovered := range []bool{false, true} {
				t.Run(strconv.FormatBool(discovered), func(t *testing.T) {
					t.Parallel()

					r := dmrRuntime(nil, stubModelStore{}, map[string]latest.ModelConfig{
						"local": {Provider: "dmr", Model: "ai/qwen3", ProviderOpts: map[string]any{"context_size": value}},
					})
					if discovered {
						r.dmrModelLister = func(context.Context) ([]dmrmodels.Model, error) {
							return []dmrmodels.Model{{ID: "ai/qwen3", Metadata: &dmrmodels.Metadata{ContextWindow: 4096}}}, nil
						}
					}
					choices := r.AvailableModels(t.Context())
					require.Len(t, choices, 1)
					assert.Equal(t, min(n, int64(math.MaxInt)), int64(choices[0].ContextLimit))
					assert.Positive(t, choices[0].ContextLimit)
				})
			}
		})
	}
}
