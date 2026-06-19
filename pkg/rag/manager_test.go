package rag

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/rag/database"
	"github.com/docker/docker-agent/pkg/rag/prefetch"
	"github.com/docker/docker-agent/pkg/rag/strategy"
	"github.com/docker/docker-agent/pkg/rag/types"
)

func TestGetAbsolutePaths_WithBasePath(t *testing.T) {
	result := GetAbsolutePaths("/base", []string{"relative/file.go", "/absolute/file.go"})
	assert.Equal(t, []string{"/base/relative/file.go", "/absolute/file.go"}, result)
}

func TestGetAbsolutePaths_EmptyBasePath(t *testing.T) {
	// When basePath is empty (OCI/URL sources), relative paths should be
	// resolved against the current working directory instead of producing
	// broken paths like "relative/file.go".
	cwd, err := os.Getwd()
	require.NoError(t, err)

	result := GetAbsolutePaths("", []string{"relative/file.go", "/absolute/file.go"})

	assert.Equal(t, filepath.Join(cwd, "relative", "file.go"), result[0])
	assert.Equal(t, "/absolute/file.go", result[1])
}

func TestGetAbsolutePaths_NilInput(t *testing.T) {
	result := GetAbsolutePaths("/base", nil)
	assert.Nil(t, result)
}

type countingStrategy struct {
	calls          atomic.Int64
	results        []database.SearchResult
	resultsByQuery map[string][]database.SearchResult
}

func (s *countingStrategy) Initialize(context.Context, []string, strategy.ChunkingConfig) error {
	return nil
}

func (s *countingStrategy) Query(_ context.Context, query string, _ int, _ float64) ([]database.SearchResult, error) {
	s.calls.Add(1)
	if s.resultsByQuery != nil {
		if results, ok := s.resultsByQuery[query]; ok {
			return append([]database.SearchResult(nil), results...), nil
		}
	}
	return append([]database.SearchResult(nil), s.results...), nil
}

func (s *countingStrategy) CheckAndReindexChangedFiles(context.Context, []string, strategy.ChunkingConfig) error {
	return nil
}

func (s *countingStrategy) StartFileWatcher(context.Context, []string, strategy.ChunkingConfig) error {
	return nil
}

func (s *countingStrategy) Close() error { return nil }

func TestQueryUsesPrefetchCacheForRepeatedQuery(t *testing.T) {
	searchStrategy := &countingStrategy{results: []database.SearchResult{{
		Document:   database.Document{ID: "1", SourcePath: "docs/rag.md", Content: "doc one"},
		Similarity: 0.9,
	}}}
	m, err := New(t.Context(), "test", Config{
		Results:        ResultsConfig{Limit: 15},
		PrefetchConfig: prefetch.Config{Enabled: true, MaxEntries: 4},
		StrategyConfigs: []strategy.Config{{
			Name:      "counting",
			Strategy:  searchStrategy,
			Limit:     5,
			Threshold: 0.5,
		}},
	}, nil)
	require.NoError(t, err)

	first, err := m.Query(t.Context(), "RAG cache")
	require.NoError(t, err)
	require.Len(t, first, 1)
	first[0].Document.Content = "caller mutation"

	second, err := m.Query(t.Context(), " rag   cache ")
	require.NoError(t, err)
	require.Len(t, second, 1)

	assert.Equal(t, int64(1), searchStrategy.calls.Load())
	assert.Equal(t, "doc one", second[0].Document.Content)
}

func TestManagerClearsPrefetchCacheOnIndexingCompleteEvent(t *testing.T) {
	events := make(chan types.Event, 1)
	m, err := New(t.Context(), "test", Config{
		Results:        ResultsConfig{Limit: 15},
		PrefetchConfig: prefetch.Config{Enabled: true, MaxEntries: 4},
		StrategyConfigs: []strategy.Config{{
			Name:     "counting",
			Strategy: &countingStrategy{},
		}},
	}, events)
	require.NoError(t, err)

	m.prefetcher.Store("RAG cache", []database.SearchResult{{
		Document:   database.Document{ID: "1", SourcePath: "docs/rag.md", Content: "stale"},
		Similarity: 0.9,
	}})

	events <- types.Event{Type: types.EventTypeIndexingComplete}

	require.Eventually(t, func() bool {
		_, ok := m.prefetcher.Get("RAG cache")
		return !ok
	}, time.Second, 10*time.Millisecond)
}

func TestManagerClearsTopologyPriorOnIndexingCompleteEvent(t *testing.T) {
	events := make(chan types.Event, 1)
	m, err := New(t.Context(), "test", Config{
		TopologyPriorConfig: TopologyPriorConfig{Enabled: true, Weight: 0.05},
		StrategyConfigs: []strategy.Config{{
			Name:     "counting",
			Strategy: &countingStrategy{},
		}},
	}, events)
	require.NoError(t, err)

	m.topologyPrior.Observe("how does rag manager query work", []database.SearchResult{{
		Document:   database.Document{ID: "1", SourcePath: "pkg/rag/manager.go", Content: "old"},
		Similarity: 0.91,
	}})

	events <- types.Event{Type: types.EventTypeIndexingComplete}

	require.Eventually(t, func() bool {
		got := m.topologyPrior.Apply("rag manager cache behavior", []database.SearchResult{
			{Document: database.Document{ID: "2", SourcePath: "pkg/model/provider/client.go", Content: "provider"}, Similarity: 0.72},
			{Document: database.Document{ID: "3", SourcePath: "pkg/rag/manager.go", Content: "manager"}, Similarity: 0.70},
		})
		return got[0].Document.SourcePath == "pkg/model/provider/client.go"
	}, time.Second, 10*time.Millisecond)
}

func TestTopologyPriorReranksOnlyFreshCurrentQueryResults(t *testing.T) {
	searchStrategy := &countingStrategy{resultsByQuery: map[string][]database.SearchResult{
		"how does rag manager query work": {
			{
				Document:   database.Document{ID: "1", SourcePath: "pkg/rag/manager.go", Content: "manager"},
				Similarity: 0.91,
			},
		},
		"rag manager cache behavior": {
			{
				Document:   database.Document{ID: "2", SourcePath: "pkg/model/provider/client.go", Content: "provider"},
				Similarity: 0.72,
			},
			{
				Document:   database.Document{ID: "3", SourcePath: "pkg/rag/manager.go", Content: "manager current"},
				Similarity: 0.70,
			},
		},
	}}
	m, err := New(t.Context(), "test", Config{
		Results:             ResultsConfig{Limit: 15},
		TopologyPriorConfig: TopologyPriorConfig{Enabled: true, Weight: 0.05, MaxSourceHistory: 8},
		StrategyConfigs: []strategy.Config{{
			Name:      "counting",
			Strategy:  searchStrategy,
			Limit:     5,
			Threshold: 0.5,
		}},
	}, nil)
	require.NoError(t, err)

	_, err = m.Query(t.Context(), "how does rag manager query work")
	require.NoError(t, err)

	got, err := m.Query(t.Context(), "rag manager cache behavior")
	require.NoError(t, err)

	require.Len(t, got, 2)
	assert.Equal(t, int64(2), searchStrategy.calls.Load())
	assert.Equal(t, "pkg/rag/manager.go", got[0].Document.SourcePath)
	assert.Equal(t, "manager current", got[0].Document.Content)
}
