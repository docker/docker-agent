package rag

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/rag/database"
	"github.com/docker/docker-agent/pkg/rag/strategy"
	"github.com/docker/docker-agent/pkg/rag/types"
)

func TestGetAbsolutePaths_WithBasePath(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	absolute := filepath.Join(t.TempDir(), "absolute", "file.go")
	result := GetAbsolutePaths(base, []string{"relative/file.go", absolute})
	assert.Equal(t, []string{filepath.Join(base, "relative", "file.go"), absolute}, result)
}

func TestGetAbsolutePaths_EmptyBasePath(t *testing.T) {
	t.Parallel()
	// When basePath is empty (OCI/URL sources), relative paths should be
	// resolved against the current working directory instead of producing
	// broken paths like "relative/file.go".
	cwd, err := os.Getwd()
	require.NoError(t, err)

	absolute := filepath.Join(t.TempDir(), "absolute", "file.go")
	result := GetAbsolutePaths("", []string{"relative/file.go", absolute})

	assert.Equal(t, filepath.Join(cwd, "relative", "file.go"), result[0])
	assert.Equal(t, absolute, result[1])
}

func TestGetAbsolutePaths_NilInput(t *testing.T) {
	t.Parallel()
	result := GetAbsolutePaths(t.TempDir(), nil)
	assert.Nil(t, result)
}

type mockStrategy struct {
	usage types.Usage
}

func (m *mockStrategy) Initialize(ctx context.Context, docPaths []string, chunking strategy.ChunkingConfig) error {
	return nil
}

func (m *mockStrategy) Query(ctx context.Context, query string, numResults int, threshold float64) ([]database.SearchResult, types.Usage, error) {
	return nil, m.usage, nil
}

func (m *mockStrategy) CheckAndReindexChangedFiles(ctx context.Context, docPaths []string, chunking strategy.ChunkingConfig) error {
	return nil
}

func (m *mockStrategy) WatchFiles(ctx context.Context, docPaths []string, chunking strategy.ChunkingConfig) error {
	return nil
}

func (m *mockStrategy) Close() error {
	return nil
}

func TestManager_Query_UsageAggregation(t *testing.T) {
	events := make(chan types.Event)
	defer close(events)

	stratA := &mockStrategy{
		usage: types.Usage{
			TotalTokens: 10,
			Cost:        0.1,
			ModelID:     "model-a",
		},
	}
	stratB := &mockStrategy{
		usage: types.Usage{
			TotalTokens: 20,
			Cost:        0.2,
			ModelID:     "model-b",
		},
	}

	cfg := Config{
		StrategyConfigs: []strategy.Config{
			{Name: "stratA", Strategy: stratA},
			{Name: "stratB", Strategy: stratB},
		},
		Results: ResultsConfig{
			Limit: 10,
		},
	}

	mgr, err := New(t.Context(), "test-rag", cfg, events)
	require.NoError(t, err)

	_, usage, err := mgr.Query(t.Context(), "test query")
	require.NoError(t, err)

	assert.Equal(t, int64(30), usage.TotalTokens)
	assert.InDelta(t, 0.3, usage.Cost, 0.0001)
	assert.Contains(t, usage.ModelID, "model-a")
	assert.Contains(t, usage.ModelID, "model-b")
}

// blockingStrategy deliberately ignores ctx and blocks Initialize and Query
// until release is closed.
type blockingStrategy struct {
	started chan struct{}
	release <-chan struct{}
}

func newBlockingStrategy(release <-chan struct{}) *blockingStrategy {
	return &blockingStrategy{started: make(chan struct{}), release: release}
}

func (s *blockingStrategy) block() {
	close(s.started)
	<-s.release
}

func (s *blockingStrategy) Initialize(context.Context, []string, strategy.ChunkingConfig) error {
	s.block()
	return nil
}

func (s *blockingStrategy) Query(context.Context, string, int, float64) ([]database.SearchResult, types.Usage, error) {
	s.block()
	return nil, types.Usage{}, nil
}

func (s *blockingStrategy) CheckAndReindexChangedFiles(context.Context, []string, strategy.ChunkingConfig) error {
	return nil
}

func (s *blockingStrategy) WatchFiles(context.Context, []string, strategy.ChunkingConfig) error {
	return nil
}

func (s *blockingStrategy) Close() error { return nil }

func TestInitializeReturnsOnContextCancellationWithBlockedStrategy(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		blocked := newBlockingStrategy(release)

		cfg := Config{
			StrategyConfigs: []strategy.Config{{Name: "blocked", Strategy: blocked}},
		}
		m, err := New(t.Context(), "test", cfg, nil)
		require.NoError(t, err)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		errCh := make(chan error, 1)
		go func() { errCh <- m.Initialize(ctx) }()

		<-blocked.started
		cancel()

		select {
		case err := <-errCh:
			require.ErrorIs(t, err, context.Canceled)
		case <-time.After(2 * time.Second):
			t.Fatal("Initialize did not return after context cancellation")
		}
	})
}

func TestQueryReturnsOnContextCancellationWithBlockedStrategy(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		first := newBlockingStrategy(release)
		second := newBlockingStrategy(release)

		// Two strategies so Query takes the multi-strategy fan-in path.
		cfg := Config{
			StrategyConfigs: []strategy.Config{
				{Name: "first", Strategy: first, Limit: 5},
				{Name: "second", Strategy: second, Limit: 5},
			},
		}
		m, err := New(t.Context(), "test", cfg, nil)
		require.NoError(t, err)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		type queryResult struct {
			results []database.SearchResult
			err     error
		}
		resCh := make(chan queryResult, 1)
		go func() {
			results, _, err := m.Query(ctx, "some query")
			resCh <- queryResult{results: results, err: err}
		}()

		<-first.started
		<-second.started
		cancel()

		select {
		case res := <-resCh:
			require.ErrorIs(t, res.err, context.Canceled)
			assert.Nil(t, res.results)
		case <-time.After(2 * time.Second):
			t.Fatal("Query did not return after context cancellation")
		}
	})
}
