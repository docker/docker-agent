package strategy

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/rag/database"
)

// Two stores over one database model several agents referencing the same RAG
// (https://github.com/docker/docker-agent/issues/4637).
func TestSharedDB_SecondStoreSkipsFileAlreadyReindexed(t *testing.T) {
	t.Parallel()
	docPaths := writeTestDocs(t, t.TempDir(), 1)
	chunking := ChunkingConfig{Size: 1024}

	db := newFakeVectorDB()
	fakeA, fakeB := &fakeEmbeddingProvider{}, &fakeEmbeddingProvider{}
	storeA, storeB := newVectorStoreWithDB(fakeA, db), newVectorStoreWithDB(fakeB, db)
	require.NoError(t, storeA.Initialize(t.Context(), docPaths, chunking))
	require.NoError(t, storeB.Initialize(t.Context(), docPaths, chunking))
	require.Equal(t, int64(0), fakeB.calls.Load(), "B must reuse what A indexed at startup")

	require.NoError(t, os.WriteFile(docPaths[0], []byte("changed content"), 0o644))

	require.NoError(t, storeA.CheckAndReindexChangedFiles(t.Context(), docPaths, chunking))
	require.Equal(t, int64(2), fakeA.calls.Load())

	require.NoError(t, storeB.CheckAndReindexChangedFiles(t.Context(), docPaths, chunking))
	assert.Equal(t, int64(0), fakeB.calls.Load(), "B must not re-embed a file A already committed")
}

func TestNeedsIndexing_DBFallbackIgnoresZeroChunkMetadata(t *testing.T) {
	t.Parallel()
	path := writeTestDocs(t, t.TempDir(), 1)[0]
	hash, err := fileSource{}.hash(path)
	require.NoError(t, err)

	db := newFakeVectorDB()
	db.metadata[path] = database.FileMetadata{SourcePath: path, FileHash: hash, ChunkCount: 0}
	store := newVectorStoreWithDB(&fakeEmbeddingProvider{}, db)

	needsIndexing, err := store.needsIndexing(t.Context(), fileSource{}, path)
	require.NoError(t, err)
	assert.True(t, needsIndexing, "a zero-chunk row with a matching hash must not count as indexed")
}
