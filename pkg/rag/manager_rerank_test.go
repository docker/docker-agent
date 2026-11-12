package rag

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/modelerrors"
	"github.com/docker/docker-agent/pkg/rag/database"
	"github.com/docker/docker-agent/pkg/rag/strategy"
	"github.com/docker/docker-agent/pkg/rag/types"
)

// failingReranker counts calls and always fails with a fixed error.
type failingReranker struct {
	calls atomic.Int64
	err   error
}

func (r *failingReranker) Rerank(context.Context, string, []database.SearchResult) ([]database.SearchResult, error) {
	r.calls.Add(1)
	return nil, r.err
}

// staticStrategy returns fixed results for every query.
type staticStrategy struct {
	results []database.SearchResult
}

func (s *staticStrategy) Initialize(context.Context, []string, strategy.ChunkingConfig) error {
	return nil
}

func (s *staticStrategy) Query(context.Context, string, int, float64) ([]database.SearchResult, types.Usage, error) {
	return s.results, types.Usage{}, nil
}

func (s *staticStrategy) CheckAndReindexChangedFiles(context.Context, []string, strategy.ChunkingConfig) error {
	return nil
}

func (s *staticStrategy) WatchFiles(context.Context, []string, strategy.ChunkingConfig) error {
	return nil
}

func (s *staticStrategy) Close() error { return nil }

func newRerankTestManager(t *testing.T, rerankErr error) (*Manager, *failingReranker) {
	t.Helper()

	results := []database.SearchResult{
		{Document: database.Document{ID: "1", Content: "doc one"}, Similarity: 0.9},
		{Document: database.Document{ID: "2", Content: "doc two"}, Similarity: 0.8},
	}

	reranker := &failingReranker{err: rerankErr}
	cfg := Config{
		StrategyConfigs: []strategy.Config{{
			Name:     "static",
			Strategy: &staticStrategy{results: results},
			Limit:    5,
		}},
		Results: ResultsConfig{
			RerankingConfig: &RerankingConfig{Reranker: reranker},
		},
	}

	m, err := New(t.Context(), "test", cfg, nil)
	require.NoError(t, err)
	return m, reranker
}

func TestQueryDisablesRerankerAfterNonRetryableError(t *testing.T) {
	t.Parallel()
	rerankErr := &modelerrors.StatusError{
		StatusCode: http.StatusNotFound,
		Err:        errors.New("not_found_error: model: claude-sonnet-4-7"),
	}
	m, reranker := newRerankTestManager(t, rerankErr)

	for range 3 {
		results, _, err := m.Query(t.Context(), "some query")
		require.NoError(t, err, "rerank failures must not fail the query")
		assert.Len(t, results, 2, "original results are returned as fallback")
	}

	assert.Equal(t, int64(1), reranker.calls.Load(),
		"reranker must be disabled after the first non-retryable error instead of being called on every query")
}

func TestQueryKeepsRerankerOnTransientError(t *testing.T) {
	t.Parallel()
	rerankErr := &modelerrors.StatusError{
		StatusCode: http.StatusInternalServerError,
		Err:        errors.New("server error"),
	}
	m, reranker := newRerankTestManager(t, rerankErr)

	for range 3 {
		results, _, err := m.Query(t.Context(), "some query")
		require.NoError(t, err)
		assert.Len(t, results, 2)
	}

	assert.Equal(t, int64(3), reranker.calls.Load(),
		"transient errors should not disable the reranker")
}

func TestQueryKeepsRerankerOnContextCancellation(t *testing.T) {
	t.Parallel()
	m, reranker := newRerankTestManager(t, context.Canceled)

	results, _, err := m.Query(t.Context(), "some query")
	require.NoError(t, err)
	assert.Len(t, results, 2)
	assert.Equal(t, int64(1), reranker.calls.Load())
	assert.False(t, m.rerankDisabled.Load(),
		"context cancellation must not permanently disable the reranker")
}
