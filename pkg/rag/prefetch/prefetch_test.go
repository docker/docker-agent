package prefetch

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/rag/database"
)

func TestDisabledPrefetcher(t *testing.T) {
	assert.Nil(t, New(Config{}))
}

func TestStoreGetAndEvictOldest(t *testing.T) {
	p := New(Config{Enabled: true, MaxEntries: 2})

	p.Store("Alpha Query", result("a.go", 0.9))
	p.Store("Beta Query", result("b.go", 0.9))
	p.Store("Gamma Query", result("c.go", 0.9))

	_, ok := p.Get("alpha query")
	assert.False(t, ok)
	_, ok = p.Get("beta query")
	assert.True(t, ok)
	got, ok := p.Get("GAMMA   query")
	require.True(t, ok)
	assert.Equal(t, "c.go", got[0].Document.SourcePath)
}

func TestCandidatesUseStableTopologyAndSourceNames(t *testing.T) {
	p := New(Config{Enabled: true, MaxCandidates: 2, MinSimilarity: 0.5, DriftThreshold: 1})
	results := []database.SearchResult{
		result("pkg/rag/manager.go", 0.9)[0],
		result("pkg/rag/vector_store.go", 0.7)[0],
		result("pkg/rag/weak.go", 0.1)[0],
	}

	p.Observe("RAG manager", results)
	p.Observe("RAG manager cache", results)

	assert.Equal(t, []string{"rag manager vector store"}, p.Candidates("RAG manager", results))
}

func TestGetUsesTopologyForRelatedFollowupQuery(t *testing.T) {
	p := New(Config{Enabled: true})
	p.Store("how does rag manager query work", []database.SearchResult{
		result("pkg/rag/manager.go", 0.92)[0],
		result("pkg/rag/prefetch/prefetch.go", 0.76)[0],
	})

	got, ok := p.Get("rag manager cache behavior")

	require.True(t, ok)
	require.Len(t, got, 2)
	assert.Equal(t, "pkg/rag/manager.go", got[0].Document.SourcePath)
}

func TestGetDoesNotUseTopologyAcrossUnrelatedQueries(t *testing.T) {
	p := New(Config{Enabled: true})
	p.Store("docker model provider auth config validation", []database.SearchResult{
		result("pkg/model/provider/anthropic/federation/federation.go", 0.86)[0],
		result("pkg/config/latest/auth.go", 0.78)[0],
	})

	_, ok := p.Get("tui message rendering scroll behavior")

	assert.False(t, ok)
}

func TestCandidatesSuppressedWhenDrifting(t *testing.T) {
	p := New(Config{Enabled: true, DriftThreshold: 0.0001})
	results := result("pkg/rag/manager.go", 0.9)

	p.Observe("short", results)
	p.Observe("this is a completely different and much longer query with more tokens", results)

	assert.Empty(t, p.Candidates("short", results))
}

func TestPrefetchDeduplicatesInFlightAndStoresResult(t *testing.T) {
	p := New(Config{Enabled: true, Timeout: time.Second})
	var calls atomic.Int64
	done := make(chan struct{})

	fetch := func(context.Context, string) ([]database.SearchResult, error) {
		calls.Add(1)
		close(done)
		return result("pkg/rag/manager.go", 0.9), nil
	}

	p.Prefetch(t.Context(), "RAG manager", fetch)
	p.Prefetch(t.Context(), "rag   manager", fetch)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("prefetch did not run")
	}
	require.Eventually(t, func() bool {
		_, ok := p.Get("rag manager")
		return ok
	}, time.Second, 10*time.Millisecond)
	assert.Equal(t, int64(1), calls.Load())
}

func TestPrefetchSurvivesForegroundContextCancellation(t *testing.T) {
	p := New(Config{Enabled: true, Timeout: time.Second})
	ctx, cancel := context.WithCancel(t.Context())
	started := make(chan struct{})
	allowReturn := make(chan struct{})

	fetch := func(ctx context.Context, _ string) ([]database.SearchResult, error) {
		close(started)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-allowReturn:
			return result("pkg/rag/manager.go", 0.9), nil
		}
	}

	p.Prefetch(ctx, "RAG manager", fetch)
	<-started
	cancel()
	close(allowReturn)

	require.Eventually(t, func() bool {
		_, ok := p.Get("rag manager")
		return ok
	}, time.Second, 10*time.Millisecond)
}

func TestReplayHitRatesTopologyBeatsExactRepeat(t *testing.T) {
	trace := replayTrace()

	exact := replayExact(trace)
	topology := replayTopology(trace)

	assert.Equal(t, replayMetrics{exactHits: 2, topologyHits: 0, misses: 8}, exact)
	assert.Equal(t, replayMetrics{exactHits: 2, topologyHits: 2, misses: 6}, topology)
	assert.Greater(t, topology.hitRate(len(trace)), exact.hitRate(len(trace)))
}

func BenchmarkReplayHitRates(b *testing.B) {
	trace := replayTrace()

	b.Run("exact-repeat", func(b *testing.B) {
		var metrics replayMetrics
		for range b.N {
			metrics = replayExact(trace)
		}
		b.ReportMetric(metrics.hitRate(len(trace))*100, "hit_percent")
		b.ReportMetric(float64(metrics.misses), "retrievals/op")
	})

	b.Run("topology-assisted", func(b *testing.B) {
		var metrics replayMetrics
		for range b.N {
			metrics = replayTopology(trace)
		}
		b.ReportMetric(metrics.hitRate(len(trace))*100, "hit_percent")
		b.ReportMetric(float64(metrics.misses), "retrievals/op")
		b.ReportMetric(float64(metrics.topologyHits), "topology_hits/op")
	})
}

func result(path string, similarity float64) []database.SearchResult {
	return []database.SearchResult{{
		Document: database.Document{
			SourcePath: path,
			Content:    "content",
		},
		Similarity: similarity,
	}}
}

type replayTurn struct {
	query   string
	results []database.SearchResult
}

type replayMetrics struct {
	exactHits    int
	topologyHits int
	misses       int
}

func (m replayMetrics) hitRate(total int) float64 {
	return float64(m.exactHits+m.topologyHits) / float64(total)
}

func replayExact(trace []replayTurn) replayMetrics {
	cache := map[string]struct{}{}
	var metrics replayMetrics
	for _, turn := range trace {
		key := normalize(turn.query)
		if _, ok := cache[key]; ok {
			metrics.exactHits++
			continue
		}
		metrics.misses++
		cache[key] = struct{}{}
	}
	return metrics
}

func replayTopology(trace []replayTurn) replayMetrics {
	p := New(Config{Enabled: true})
	exactSeen := map[string]struct{}{}
	var metrics replayMetrics
	for _, turn := range trace {
		key := normalize(turn.query)
		if _, ok := exactSeen[key]; ok {
			metrics.exactHits++
			continue
		}
		if _, ok := p.Get(turn.query); ok {
			metrics.topologyHits++
			exactSeen[key] = struct{}{}
			continue
		}
		metrics.misses++
		exactSeen[key] = struct{}{}
		p.Store(turn.query, turn.results)
	}
	return metrics
}

func replayTrace() []replayTurn {
	return []replayTurn{
		{
			query: "how does rag manager query work",
			results: []database.SearchResult{
				result("pkg/rag/manager.go", 0.92)[0],
				result("pkg/rag/prefetch/prefetch.go", 0.76)[0],
			},
		},
		{
			query: "rag manager cache behavior",
			results: []database.SearchResult{
				result("pkg/rag/manager.go", 0.89)[0],
				result("pkg/rag/prefetch/prefetch.go", 0.81)[0],
			},
		},
		{
			query: "how does rag manager query work",
			results: []database.SearchResult{
				result("pkg/rag/manager.go", 0.92)[0],
				result("pkg/rag/prefetch/prefetch.go", 0.76)[0],
			},
		},
		{
			query: "prefetch drift threshold behavior",
			results: []database.SearchResult{
				result("pkg/rag/prefetch/prefetch.go", 0.91)[0],
				result("pkg/rag/prefetch/prefetch_test.go", 0.84)[0],
			},
		},
		{
			query: "background prefetch should survive turn cancellation",
			results: []database.SearchResult{
				result("pkg/rag/prefetch/prefetch.go", 0.88)[0],
				result("pkg/tools/builtin/agent/agent.go", 0.67)[0],
			},
		},
		{
			query: "how are rag documents reindexed after file changes",
			results: []database.SearchResult{
				result("pkg/rag/strategy/vector_store.go", 0.9)[0],
				result("pkg/rag/strategy/bm25.go", 0.82)[0],
			},
		},
		{
			query: "docker model provider auth config validation",
			results: []database.SearchResult{
				result("pkg/model/provider/anthropic/federation/federation.go", 0.86)[0],
				result("pkg/config/latest/auth.go", 0.78)[0],
			},
		},
		{
			query: "anthropic auth config validation",
			results: []database.SearchResult{
				result("pkg/config/latest/auth.go", 0.83)[0],
				result("pkg/model/provider/anthropic/federation/federation.go", 0.79)[0],
			},
		},
		{
			query: "docker model provider auth config validation",
			results: []database.SearchResult{
				result("pkg/model/provider/anthropic/federation/federation.go", 0.86)[0],
				result("pkg/config/latest/auth.go", 0.78)[0],
			},
		},
		{
			query: "tui message rendering scroll behavior",
			results: []database.SearchResult{
				result("pkg/tui/components/messages/messages.go", 0.88)[0],
				result("pkg/tui/components/scrollview/scrollview.go", 0.74)[0],
			},
		},
	}
}
