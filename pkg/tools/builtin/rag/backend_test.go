package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/rag"
	"github.com/docker/docker-agent/pkg/rag/database"
	ragtypes "github.com/docker/docker-agent/pkg/rag/types"
)

type stubBackend struct {
	results    []database.SearchResult
	usage      ragtypes.Usage
	query      string
	err        error
	events     chan ragtypes.Event
	initialize func(context.Context) error
	watch      func(context.Context) error
	close      func() error
}

func (b *stubBackend) Initialize(ctx context.Context) error {
	if b.initialize != nil {
		return b.initialize(ctx)
	}
	return nil
}

func (b *stubBackend) StartFileWatcher(ctx context.Context) error {
	if b.watch != nil {
		return b.watch(ctx)
	}
	return nil
}
func (b *stubBackend) Events() <-chan ragtypes.Event { return b.events }
func (b *stubBackend) Query(_ context.Context, query string) ([]database.SearchResult, ragtypes.Usage, error) {
	b.query = query
	return b.results, b.usage, b.err
}
func (*stubBackend) Description() string     { return "backend description" }
func (*stubBackend) ToolInstruction() string { return "backend instruction" }
func (b *stubBackend) Close() error {
	if b.close != nil {
		return b.close()
	}
	return nil
}

func TestBackendQuery(t *testing.T) {
	t.Parallel()
	b := &stubBackend{}
	for i := range 12 {
		b.results = append(b.results, database.SearchResult{Document: database.Document{SourcePath: fmt.Sprintf("%d.txt", i), Content: fmt.Sprintf("content %d", i), ChunkIndex: i}, Similarity: float64(i) / 12})
	}
	tool := New(b, "search")
	list, err := tool.Tools(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "backend description", list[0].Description)
	assert.Equal(t, "backend instruction", tool.Instructions())
	result, err := tool.handleQueryRAG(t.Context(), queryRAGArgs{Query: "needle"})
	require.NoError(t, err)
	assert.Equal(t, "needle", b.query)
	var results []queryResult
	require.NoError(t, json.Unmarshal([]byte(result.Output), &results))
	require.Len(t, results, 10)
	assert.Equal(t, "11.txt", results[0].SourcePath)
	assert.Equal(t, "content 11", results[0].Content)
	assert.Equal(t, 11, results[0].ChunkIndex)
	assert.Equal(t, "2.txt", results[9].SourcePath)
	sentinel := errors.New("query failed")
	b.err = sentinel
	_, err = tool.handleQueryRAG(t.Context(), queryRAGArgs{Query: "needle"})
	require.ErrorIs(t, err, sentinel)
	b.query = ""
	_, err = tool.handleQueryRAG(t.Context(), queryRAGArgs{})
	require.EqualError(t, err, "query cannot be empty")
	assert.Empty(t, b.query)
}

func TestBackendLifecycle(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		watchContexts := make(chan context.Context, 1)
		watcherDone := false
		closed := false
		b := &stubBackend{events: make(chan ragtypes.Event, 1)}
		b.initialize = func(ctx context.Context) error {
			require.NoError(t, ctx.Err())
			b.events <- ragtypes.Event{Type: ragtypes.EventTypeIndexingStarted}
			return nil
		}
		b.watch = func(ctx context.Context) error {
			watchContexts <- ctx
			<-ctx.Done()
			watcherDone = true
			return ctx.Err()
		}
		b.close = func() error {
			assert.True(t, watcherDone)
			closed = true
			return nil
		}
		tool := New(b, "search")
		received := make(chan ragtypes.Event, 1)
		unsubscribe := tool.SubscribeEvents(func(event ragtypes.Event) { received <- event })
		defer unsubscribe()
		ctx, cancel := context.WithCancel(t.Context())
		require.NoError(t, tool.Start(ctx))
		cancel()
		synctest.Wait()
		watchCtx := <-watchContexts
		require.NotNil(t, watchCtx)
		require.NoError(t, watchCtx.Err())
		assert.Equal(t, ragtypes.EventTypeIndexingStarted, (<-received).Type)
		require.NoError(t, tool.Stop(t.Context()))
		assert.True(t, closed)
		assert.ErrorIs(t, watchCtx.Err(), context.Canceled)
	})
}

func TestNilBackend(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		manager backend
	}{{"nil", nil}, {"typed nil", (*rag.Manager)(nil)}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tool := New(tc.manager, "search")
			assert.Nil(t, tool.manager)
			require.NoError(t, tool.Start(t.Context()))
			require.NoError(t, tool.Stop(t.Context()))
			list, err := tool.Tools(t.Context())
			require.NoError(t, err)
			assert.Contains(t, list[0].Description, "search")
			assert.Contains(t, tool.Instructions(), "search")
		})
	}
}
