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

	assert.Equal(t, []string{"rag manager manager", "rag manager vector_store"}, p.Candidates("RAG manager", results))
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

func result(path string, similarity float64) []database.SearchResult {
	return []database.SearchResult{{
		Document: database.Document{
			SourcePath: path,
			Content:    "content",
		},
		Similarity: similarity,
	}}
}
