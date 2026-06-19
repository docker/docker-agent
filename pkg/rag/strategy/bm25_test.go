package strategy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/rag/types"
)

func TestBM25LiveReindexEmitsIndexingComplete(t *testing.T) {
	events := make(chan types.Event, 16)
	dir := t.TempDir()
	docPath := filepath.Join(dir, "doc.txt")
	require.NoError(t, os.WriteFile(docPath, []byte("initial blork content"), 0o644))

	db, err := newBM25DB(filepath.Join(dir, "bm25.db"), "bm25")
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	strategy := newBM25Strategy("bm25", db, events, 1.5, 0.75, ChunkingConfig{Size: 1024}, nil)
	require.NoError(t, strategy.Initialize(t.Context(), []string{docPath}, ChunkingConfig{Size: 1024}))
	drainEvents(events)

	require.NoError(t, os.WriteFile(docPath, []byte("updated blork content"), 0o644))

	indexed := strategy.reindexChangedFiles(t.Context(), []string{docPath}, []string{docPath})

	assert.Equal(t, 1, indexed)
	assertEventType(t, events, types.EventTypeIndexingComplete)
}

func drainEvents(events <-chan types.Event) {
	for {
		select {
		case <-events:
		default:
			return
		}
	}
}

func assertEventType(t *testing.T, events <-chan types.Event, want types.EventTye) {
	t.Helper()
	for range cap(events) {
		select {
		case event := <-events:
			if event.Type == want {
				return
			}
		default:
			t.Fatalf("event %q was not emitted", want)
		}
	}
	t.Fatalf("event %q was not emitted", want)
}
