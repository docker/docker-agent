package modelcatalog

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

// stubStore is a Store backed by in-memory data, used to verify
// which of the gateway / catalog code paths populated the picker.
type stubStore struct {
	db     *modelsdev.Database
	models map[string]*modelsdev.Model
}

func (s stubStore) GetModel(_ context.Context, id modelsdev.ID) (*modelsdev.Model, error) {
	if m, ok := s.models[id.String()]; ok {
		return m, nil
	}
	return nil, errors.New("model not found")
}

func (s stubStore) GetDatabase(context.Context) (*modelsdev.Database, error) {
	if s.db == nil {
		return nil, errors.New("no database")
	}
	return s.db, nil
}

// gatewayRuntime builds a LocalRuntime wired to the given gateway URL with
// a Docker token available (httptest servers are localhost, hence trusted).
// A direct OpenAI credential is also set: the catalog fallback taken when
// gateway discovery fails is credential-based, not gateway-based.
func gatewayRuntime(gatewayURL string, store Store) *Catalog {
	return &Catalog{
		Store: store,
		Config: &Config{
			ModelsGateway: gatewayURL,
			EnvProvider: environment.NewMapEnvProvider(map[string]string{
				environment.DockerDesktopTokenEnv: "test-token",
				"OPENAI_API_KEY":                  "sk-test",
			}),
			Models: map[string]latest.ModelConfig{
				"root_model": {Provider: "anthropic", Model: "claude-sonnet-4-0"},
			},
		},
	}
}

// catalogDB returns a models.dev database with an entry that must only
// appear in the picker when the catalog fallback path is taken.
func catalogDB() *modelsdev.Database {
	return &modelsdev.Database{
		Providers: map[string]modelsdev.Provider{
			"openai": {
				Models: map[string]modelsdev.Model{
					"catalog-only-model": {
						Name:       "Catalog Only",
						Modalities: modelsdev.Modalities{Output: []string{"text"}},
					},
				},
			},
		},
	}
}

func refs(choices []ModelChoice) []string {
	out := make([]string, 0, len(choices))
	for _, c := range choices {
		out = append(out, c.Ref)
	}
	return out
}

func TestAvailableModels_GatewayDiscovery(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[
			{"id":"openai/gpt-4o"},
			{"id":"anthropic/claude-sonnet-4-0"},
			{"id":"bare-model"},
			{"id":"openai/text-embedding-3-small"}
		]}`))
	}))
	defer server.Close()

	r := gatewayRuntime(server.URL, stubStore{db: catalogDB()})

	choices := r.AvailableModels(t.Context(), "")
	got := refs(choices)

	assert.Contains(t, got, "root_model", "configured models must always be listed")
	assert.Contains(t, got, "openai/gpt-4o")
	assert.Contains(t, got, "openai/bare-model", "bare IDs must be routed through the openai provider")
	assert.NotContains(t, got, "anthropic/claude-sonnet-4-0", "gateway models duplicating configured ones must be skipped")
	assert.NotContains(t, got, "openai/text-embedding-3-small", "embedding models must be filtered out")
	assert.NotContains(t, got, "openai/catalog-only-model", "catalog must be suppressed when gateway discovery succeeds")

	for _, c := range choices {
		if c.Ref == "openai/gpt-4o" {
			assert.True(t, c.IsCatalog, "gateway models should be grouped like catalog entries")
		}
	}
}

func TestAvailableModels_GatewayDiscoveryMetadata(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"openai/gpt-4o"}]}`))
	}))
	defer server.Close()

	store := stubStore{models: map[string]*modelsdev.Model{
		"openai/gpt-4o": {
			Name:  "GPT-4o",
			Cost:  &modelsdev.Cost{Input: 2.5, Output: 10},
			Limit: modelsdev.Limit{Context: 128000, Output: 16384},
		},
	}}
	r := gatewayRuntime(server.URL, store)

	choices := r.AvailableModels(t.Context(), "")

	var found *ModelChoice
	for i := range choices {
		if choices[i].Ref == "openai/gpt-4o" {
			found = &choices[i]
		}
	}
	require.NotNil(t, found)
	assert.Equal(t, "GPT-4o", found.Name)
	assert.InEpsilon(t, 2.5, found.InputCost, 0.001)
	assert.Equal(t, 128000, found.ContextLimit)
}

func TestAvailableModels_GatewayUnsupportedFallsBackToCatalog(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer server.Close()

	r := gatewayRuntime(server.URL, stubStore{db: catalogDB()})

	got := refs(r.AvailableModels(t.Context(), ""))

	assert.Contains(t, got, "root_model")
	assert.Contains(t, got, "openai/catalog-only-model", "catalog must be used when the gateway doesn't support /v1/models")
}

func TestAvailableModels_GatewayEmptyListFallsBackToCatalog(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer server.Close()

	r := gatewayRuntime(server.URL, stubStore{db: catalogDB()})

	got := refs(r.AvailableModels(t.Context(), ""))

	assert.Contains(t, got, "openai/catalog-only-model")
}

func TestListGatewayModels_CachesResult(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"data":[{"id":"openai/gpt-4o"}]}`))
	}))
	defer server.Close()

	r := gatewayRuntime(server.URL, stubStore{})

	_ = r.AvailableModels(t.Context(), "")
	_ = r.AvailableModels(t.Context(), "")

	assert.Equal(t, int32(1), requests.Load(), "gateway must be queried once within the cache TTL")
}

func TestListGatewayModels_CacheExpires(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"data":[{"id":"openai/gpt-4o"}]}`))
	}))
	defer server.Close()

	now := time.Now()
	r := gatewayRuntime(server.URL, stubStore{})
	r.Now = func() time.Time { return now }

	_, err := r.listGatewayModels(t.Context())
	require.NoError(t, err)

	now = now.Add(gatewayModelsTTL + time.Second)
	_, err = r.listGatewayModels(t.Context())
	require.NoError(t, err)

	assert.Equal(t, int32(2), requests.Load(), "gateway must be re-queried after the cache TTL")
}

func TestListGatewayModels_CachesFailure(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	r := gatewayRuntime(server.URL, stubStore{})

	_, err := r.listGatewayModels(t.Context())
	require.Error(t, err)
	_, err = r.listGatewayModels(t.Context())
	require.Error(t, err)

	assert.Equal(t, int32(1), requests.Load(), "failures must be cached to avoid hammering the gateway")
}

func TestListGatewayModels_DoesNotCacheCallerCancellation(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"data":[{"id":"openai/gpt-4o"}]}`))
	}))
	defer server.Close()

	r := gatewayRuntime(server.URL, stubStore{})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := r.listGatewayModels(ctx)
	require.Error(t, err)

	ids, err := r.listGatewayModels(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"openai/gpt-4o"}, ids)
	assert.Equal(t, int32(1), requests.Load(), "caller cancellation must not poison the gateway discovery cache")
}

func TestListGatewayModels_DoubleCheckAvoidsRedundantFetch(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"data":[{"id":"openai/gpt-4o"}]}`))
	}))
	defer server.Close()

	now := time.Now()
	r := gatewayRuntime(server.URL, stubStore{})
	r.Now = func() time.Time { return now }

	_, err := r.listGatewayModels(t.Context())
	require.NoError(t, err)
	require.Equal(t, int32(1), requests.Load())

	// Simulate the TOCTOU window: the caller's freshness check sees a
	// stale cache (singleflight already released the key), but by the
	// time its closure runs the cache has been repopulated. The clock
	// reads stale exactly once — for the outer check — then fresh for
	// the double-check inside the closure, which must return the cached
	// result instead of re-fetching.
	staleReads := 1
	r.Now = func() time.Time {
		if staleReads > 0 {
			staleReads--
			return now.Add(gatewayModelsTTL + time.Second)
		}
		return now
	}

	ids, err := r.listGatewayModels(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"openai/gpt-4o"}, ids)
	assert.Equal(t, int32(1), requests.Load(), "double-check inside the singleflight closure must reuse the fresh cache")
}

func TestListGatewayModels_ConcurrentCallersCoalesce(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		<-release
		_, _ = w.Write([]byte(`{"data":[{"id":"openai/gpt-4o"}]}`))
	}))
	defer server.Close()

	r := gatewayRuntime(server.URL, stubStore{})

	const callers = 8
	var wg sync.WaitGroup
	results := make([][]string, callers)

	// First caller triggers the fetch; the handler parks it on release.
	wg.Go(func() {
		results[0], _ = r.listGatewayModels(t.Context())
	})
	require.Eventually(t, func() bool { return requests.Load() >= 1 },
		time.Second, time.Millisecond, "fetch never reached the gateway")

	// The remaining callers start while the fetch is provably in flight,
	// so they must coalesce onto it (or read the cache once it lands). A
	// non-coalescing implementation would issue its own gateway requests
	// and trip the assertion below.
	for i := 1; i < callers; i++ {
		wg.Go(func() {
			results[i], _ = r.listGatewayModels(t.Context())
		})
	}
	close(release)
	wg.Wait()

	assert.Equal(t, int32(1), requests.Load(), "concurrent callers must coalesce on one in-flight request")
	for i := range callers {
		assert.Equal(t, []string{"openai/gpt-4o"}, results[i])
	}
}

func TestAvailableModels_GatewayEmbeddingFilteredByCatalogFamily(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"openai/some-vector-model"},{"id":"openai/gpt-4o"}]}`))
	}))
	defer server.Close()

	// The model ID contains no "embed" substring; only the catalog
	// Family identifies it as an embedding model.
	store := stubStore{models: map[string]*modelsdev.Model{
		"openai/some-vector-model": {Name: "Vector", Family: "text-embedding"},
	}}
	r := gatewayRuntime(server.URL, store)

	got := refs(r.AvailableModels(t.Context(), ""))

	assert.NotContains(t, got, "openai/some-vector-model", "embedding models identified by catalog family must be filtered")
	assert.Contains(t, got, "openai/gpt-4o")
}

func TestAvailableModels_GatewayNonTextFilteredByCatalogModalities(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"openai/image-gen"},{"id":"openai/gpt-4o"}]}`))
	}))
	defer server.Close()

	// The catalog declares image-only output for image-gen, so it is not a
	// chat model. gpt-4o has no catalog entry and must be kept (no signal).
	store := stubStore{models: map[string]*modelsdev.Model{
		"openai/image-gen": {Name: "Image Gen", Modalities: modelsdev.Modalities{Output: []string{"image"}}},
	}}
	r := gatewayRuntime(server.URL, store)

	got := refs(r.AvailableModels(t.Context(), ""))

	assert.NotContains(t, got, "openai/image-gen", "models whose catalog metadata declares non-text output must be filtered")
	assert.Contains(t, got, "openai/gpt-4o")
}
