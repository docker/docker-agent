package strategy

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/rag/embed"
	"github.com/docker/docker-agent/pkg/rag/types"
)

func TestMemorySource(t *testing.T) {
	t.Parallel()
	src := memorySource{"b.md": []byte("bee"), "a.md": []byte("ant")}

	paths, err := src.list(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"a.md", "b.md"}, paths, "sorted for deterministic indexing")

	content, err := src.read("a.md")
	require.NoError(t, err)
	assert.Equal(t, "ant", string(content))

	sum := sha256.Sum256([]byte("ant"))
	hash, err := src.hash("a.md")
	require.NoError(t, err)
	assert.Equal(t, hex.EncodeToString(sum[:]), hash, "same hash chunk.FileHash gives a file with that content")

	_, err = src.read("missing.md")
	require.ErrorContains(t, err, `"missing.md" was not supplied`)
	_, err = src.hash("missing.md")
	require.Error(t, err)
}

func TestBM25InitializeDocuments(t *testing.T) {
	t.Parallel()
	events := make(chan types.Event, 16)
	s := newBM25Strategy("bm25", newMemoryBM25DB(), events, 1.5, 0.75, ChunkingConfig{Size: 1024}, nil)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	docs := map[string][]byte{
		"cats.md":  []byte("cats purr and cats sleep"),
		"dogs.md":  []byte("dogs bark"),
		"empty.md": nil,
	}
	require.NoError(t, s.InitializeDocuments(t.Context(), docs, ChunkingConfig{Size: 1024}))

	results, _, err := s.Query(t.Context(), "cats", 5, 0.01)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "cats.md", results[0].Document.SourcePath)
	assert.Positive(t, s.avgDocLength, "BM25 statistics are computed like after Initialize")

	var seen []types.EventTye
	for len(events) > 0 {
		seen = append(seen, (<-events).Type)
	}
	assert.Equal(t, []types.EventTye{types.EventTypeIndexingStarted, types.EventTypeIndexingProgress, types.EventTypeIndexingProgress, types.EventTypeIndexingProgress, types.EventTypeIndexingComplete}, seen)

	// A second pass over the same documents finds nothing to do.
	require.NoError(t, s.InitializeDocuments(t.Context(), docs, ChunkingConfig{Size: 1024}))
	assert.Empty(t, events)

	// A document that disappears is cleaned up like a deleted file.
	delete(docs, "dogs.md")
	require.NoError(t, s.InitializeDocuments(t.Context(), docs, ChunkingConfig{Size: 1024}))
	all, err := s.db.GetAllFileMetadata(t.Context())
	require.NoError(t, err)
	var remaining []string
	for _, meta := range all {
		remaining = append(remaining, meta.SourcePath)
	}
	assert.Equal(t, []string{"cats.md", "empty.md"}, remaining)
}

func TestVectorStoreInitializeDocuments(t *testing.T) {
	t.Parallel()
	fake := &fakeEmbeddingProvider{}
	db := newMemoryVectorDB(2, false)
	store := NewVectorStore(VectorStoreConfig{
		Name:                 "test",
		Database:             db,
		Embedder:             embed.New(fake),
		EmbeddingConcurrency: 1,
		FileIndexConcurrency: 1,
		Chunking:             ChunkingConfig{Size: 1024},
	})
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	docs := map[string][]byte{"a.md": []byte("alpha"), "b.md": []byte("beta")}
	require.NoError(t, store.InitializeDocuments(t.Context(), docs, ChunkingConfig{Size: 1024}))
	assert.Equal(t, int64(2), fake.calls.Load())

	results, _, err := store.Query(t.Context(), "alpha", 5, 0)
	require.NoError(t, err)
	assert.Len(t, results, 2)
	assert.Equal(t, int64(3), fake.calls.Load(), "one embedding for the query")

	docs["b.md"] = []byte("beta changed")
	require.NoError(t, store.InitializeDocuments(t.Context(), docs, ChunkingConfig{Size: 1024}))
	assert.Equal(t, int64(4), fake.calls.Load(), "only the changed document is re-embedded")
}
