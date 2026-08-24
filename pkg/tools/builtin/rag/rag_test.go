package rag

import (
	"cmp"
	"context"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/rag"
	"github.com/docker/docker-agent/pkg/rag/database"
	"github.com/docker/docker-agent/pkg/rag/strategy"
	ragtypes "github.com/docker/docker-agent/pkg/rag/types"
)

func TestRAGTool_ToolName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		toolName     string
		expectedName string
	}{
		{
			name:         "Uses custom tool name",
			toolName:     "custom_search",
			expectedName: "custom_search",
		},
		{
			name:         "Uses provided name",
			toolName:     "my_docs",
			expectedName: "my_docs",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool := &ToolSet{
				toolName: tt.toolName,
				manager:  nil,
			}

			tools, err := tool.Tools(t.Context())
			require.NoError(t, err)
			require.Len(t, tools, 1)
			assert.Equal(t, tt.expectedName, tools[0].Name)
			assert.Equal(t, "knowledge", tools[0].Category)
		})
	}
}

func TestRAGTool_DefaultDescription(t *testing.T) {
	t.Parallel()
	tool := &ToolSet{
		toolName: "test_docs",
		manager:  nil,
	}

	tools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, tools, 1)
	assert.Contains(t, tools[0].Description, "test_docs")
}

func TestRAGTool_SortResults(t *testing.T) {
	t.Parallel()
	results := []queryResult{
		{SourcePath: "a.txt", Similarity: 0.5},
		{SourcePath: "b.txt", Similarity: 0.9},
		{SourcePath: "c.txt", Similarity: 0.3},
		{SourcePath: "d.txt", Similarity: 0.7},
	}

	slices.SortFunc(results, func(a, b queryResult) int {
		return cmp.Compare(b.Similarity, a.Similarity)
	})

	assert.Equal(t, "b.txt", results[0].SourcePath)
	assert.Equal(t, "d.txt", results[1].SourcePath)
	assert.Equal(t, "a.txt", results[2].SourcePath)
	assert.Equal(t, "c.txt", results[3].SourcePath)
}

type mockStrategy struct {
	usage ragtypes.Usage
}

func (m *mockStrategy) Initialize(ctx context.Context, docPaths []string, chunking strategy.ChunkingConfig) error {
	return nil
}

func (m *mockStrategy) Query(ctx context.Context, query string, numResults int, threshold float64) ([]database.SearchResult, ragtypes.Usage, error) {
	return nil, m.usage, nil
}

func (m *mockStrategy) CheckAndReindexChangedFiles(ctx context.Context, docPaths []string, chunking strategy.ChunkingConfig) error {
	return nil
}

func (m *mockStrategy) StartFileWatcher(ctx context.Context, docPaths []string, chunking strategy.ChunkingConfig) error {
	return nil
}

func (m *mockStrategy) Close() error {
	return nil
}

func TestRAGTool_HandleQuery_Telemetry(t *testing.T) {
	// This test asserts handleQueryRAG runs and doesn't panic.
	// Since telemetry is global, we can't easily assert on the emitted event here without
	// exposing internal test utilities, but we can verify it doesn't crash on non-zero usage.

	events := make(chan ragtypes.Event)
	defer close(events)

	stratA := &mockStrategy{
		usage: ragtypes.Usage{
			TotalTokens: 10,
			Cost:        0.1,
			ModelID:     "test-model",
		},
	}

	cfg := rag.Config{
		StrategyConfigs: []strategy.Config{
			{Name: "stratA", Strategy: stratA},
		},
		Results: rag.ResultsConfig{
			Limit: 10,
		},
	}

	mgr, err := rag.New(t.Context(), "test-rag", cfg, events)
	require.NoError(t, err)

	tool := &ToolSet{
		manager:  mgr,
		toolName: "test-rag",
	}

	res, err := tool.handleQueryRAG(t.Context(), queryRAGArgs{Query: "test"})
	require.NoError(t, err)
	assert.NotNil(t, res)
}

type failingMockStrategy struct {
	mockStrategy
}

func (m *failingMockStrategy) Initialize(_ context.Context, _ []string, _ strategy.ChunkingConfig) error {
	return assert.AnError
}

func TestStopAfterFailedStart(t *testing.T) {
	t.Parallel()

	strategyMock := &failingMockStrategy{}
	cfg := rag.Config{
		StrategyConfigs: []strategy.Config{
			{Name: "failingStrategy", Strategy: strategyMock},
		},
	}

	mgr, err := rag.New(t.Context(), "failing-rag", cfg, nil)
	require.NoError(t, err)

	tool := &ToolSet{
		manager:  mgr,
		toolName: "failing-rag",
	}

	err = tool.Start(t.Context())
	require.Error(t, err)

	done := make(chan struct{})
	go func() {
		_ = tool.Stop(t.Context())
		close(done)
	}()

	select {
	case <-done:
		// Success: Stop returned without deadlocking
	case <-time.After(5 * time.Second):
		t.Fatal("Stop() deadlocked after a failed Start()")
	}
}

// watcherCapturingStrategy hands the context its file watcher receives to the
// test so it can assert on the watcher's lifetime. Initialize is immediate
// (inherited from mockStrategy) and StartFileWatcher returns as soon as the
// watcher is "installed", mirroring the real strategies.
type watcherCapturingStrategy struct {
	mockStrategy

	watchCtx chan context.Context
}

func (m *watcherCapturingStrategy) StartFileWatcher(ctx context.Context, _ []string, _ strategy.ChunkingConfig) error {
	m.watchCtx <- ctx
	return nil
}

type watcherCtxKey struct{}

// TestStartDetachesWatcherFromCallerContext covers #4001: Start may run under
// a short-lived context (e.g. the runtime's bounded startup probe), and the
// long-lived file watcher must not die with it. The watcher context keeps the
// caller's values, but only Stop() owns its cancellation.
func TestStartDetachesWatcherFromCallerContext(t *testing.T) {
	t.Parallel()

	strategyMock := &watcherCapturingStrategy{watchCtx: make(chan context.Context, 1)}
	cfg := rag.Config{
		StrategyConfigs: []strategy.Config{
			{Name: "capturing", Strategy: strategyMock},
		},
	}

	mgr, err := rag.New(t.Context(), "watched-rag", cfg, nil)
	require.NoError(t, err)

	tool := &ToolSet{
		manager:  mgr,
		toolName: "watched-rag",
	}

	startCtx, cancelStart := context.WithCancel(context.WithValue(t.Context(), watcherCtxKey{}, "kept"))
	defer cancelStart()
	require.NoError(t, tool.Start(startCtx))

	var watchCtx context.Context
	select {
	case watchCtx = <-strategyMock.watchCtx:
	case <-time.After(5 * time.Second):
		t.Fatal("file watcher was never started")
	}
	assert.Equal(t, "kept", watchCtx.Value(watcherCtxKey{}), "watcher context must keep the caller's values")

	// Cancelling the Start context (probe timeout) must not kill the watcher.
	// Cancellation propagates synchronously, so this check is deterministic.
	cancelStart()
	require.NoError(t, watchCtx.Err(), "watcher must outlive the context passed to Start")

	// Stop owns the watcher's cancellation.
	require.NoError(t, tool.Stop(t.Context()))
	assert.ErrorIs(t, watchCtx.Err(), context.Canceled, "Stop must cancel the watcher")
}
