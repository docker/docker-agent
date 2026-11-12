package strategy

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/rag/database"
)

func TestBM25QueriesDuringStatisticsUpdates(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := newMemoryBM25DB()
	s := newBM25Strategy("test", db, nil, 1.5, 0.75, ChunkingConfig{Size: 1024}, nil)
	require.NoError(t, db.AddDocument(ctx, database.Document{ID: "a", SourcePath: "a.txt", Content: "searchable document content"}))
	require.NoError(t, s.calculateAvgDocLength(ctx))
	var workers sync.WaitGroup
	for range 4 {
		workers.Go(func() {
			for range 100 {
				_, _, err := s.Query(ctx, "searchable", 5, 0)
				if err != nil {
					t.Error(err)
				}
			}
		})
	}
	workers.Go(func() {
		for range 100 {
			if err := s.calculateAvgDocLength(ctx); err != nil {
				t.Error(err)
			}
		}
	})
	workers.Wait()
}
