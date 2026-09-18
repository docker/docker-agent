package strategy

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/rag/database"
)

// testVectorDimensions and testDocPath are fixed across the atomicity tests
// below - only the DB behavior under test varies.
const (
	testVectorDimensions = 3
	testDocPath          = "/a.txt"
)

// vectorDBFactory constructs a vectorStoreDB backend for the atomicity tests
// below, so they can run identically against every implementation: the
// platform constructors (sqlite natively, in-memory under js) and the
// in-memory backend explicitly, so it is covered on every platform.
type vectorDBFactory struct {
	name     string
	semantic bool // backend records embedding inputs
	new      func(ctx context.Context, dbPath string) (vectorStoreDB, error)
}

var vectorDBFactories = []vectorDBFactory{
	{
		name:     "semantic",
		semantic: true,
		new: func(ctx context.Context, dbPath string) (vectorStoreDB, error) {
			return newSemanticVectorDB(ctx, dbPath, testVectorDimensions, "semantic")
		},
	},
	{
		name: "chunked",
		new: func(ctx context.Context, dbPath string) (vectorStoreDB, error) {
			return newChunkedVectorDB(ctx, dbPath, testVectorDimensions, "chunked")
		},
	},
	{
		name:     "memory-semantic",
		semantic: true,
		new: func(context.Context, string) (vectorStoreDB, error) {
			return newMemoryVectorDB(testVectorDimensions, true), nil
		},
	},
	{
		name: "memory-chunked",
		new: func(context.Context, string) (vectorStoreDB, error) {
			return newMemoryVectorDB(testVectorDimensions, false), nil
		},
	},
}

func newTestDB(t *testing.T, factory vectorDBFactory) vectorStoreDB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := factory.new(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func docsFor(contents ...string) []database.Document {
	docs := make([]database.Document, len(contents))
	for i, c := range contents {
		docs[i] = database.Document{
			ID:         testDocPath,
			SourcePath: testDocPath,
			ChunkIndex: i,
			Content:    c,
			FileHash:   "irrelevant",
		}
	}
	return docs
}

func embeddingsFor(n int) [][]float64 {
	embeddings := make([][]float64, n)
	for i := range embeddings {
		vec := make([]float64, testVectorDimensions)
		for j := range vec {
			vec[j] = float64(i*testVectorDimensions + j)
		}
		embeddings[i] = vec
	}
	return embeddings
}

func inputsFor(prefix string, n int) []string {
	inputs := make([]string, n)
	for i := range inputs {
		inputs[i] = prefix
	}
	return inputs
}

func TestReplaceFileDocuments_WritesFileAndChunks(t *testing.T) {
	t.Parallel()
	for _, factory := range vectorDBFactories {
		t.Run(factory.name, func(t *testing.T) {
			t.Parallel()
			db := newTestDB(t, factory)
			ctx := t.Context()

			docs := docsFor("chunk one", "chunk two")
			meta := database.FileMetadata{SourcePath: testDocPath, FileHash: "hash-v1", ChunkCount: len(docs)}
			require.NoError(t, db.ReplaceFileDocuments(ctx, meta, docs, embeddingsFor(2), inputsFor("summary", 2)))

			all, err := db.GetAllFileMetadata(ctx)
			require.NoError(t, err)
			require.Len(t, all, 1)
			assert.Equal(t, "hash-v1", all[0].FileHash)
			assert.Equal(t, 2, all[0].ChunkCount)

			results, err := db.SearchSimilarVectors(ctx, embeddingsFor(1)[0], 10)
			require.NoError(t, err)
			require.Len(t, results, 2)
			gotContents := []string{results[0].Content, results[1].Content}
			assert.ElementsMatch(t, []string{"chunk one", "chunk two"}, gotContents)
		})
	}
}

func TestReplaceFileDocuments_ReplacesFewerChunksLeavesNoStaleData(t *testing.T) {
	t.Parallel()
	for _, factory := range vectorDBFactories {
		t.Run(factory.name, func(t *testing.T) {
			t.Parallel()
			db := newTestDB(t, factory)
			ctx := t.Context()

			v1 := docsFor("one", "two", "three")
			require.NoError(t, db.ReplaceFileDocuments(ctx, database.FileMetadata{SourcePath: testDocPath, FileHash: "v1", ChunkCount: 3}, v1, embeddingsFor(3), inputsFor("s", 3)))

			v2 := docsFor("only")
			require.NoError(t, db.ReplaceFileDocuments(ctx, database.FileMetadata{SourcePath: testDocPath, FileHash: "v2", ChunkCount: 1}, v2, embeddingsFor(1), inputsFor("s", 1)))

			all, err := db.GetAllFileMetadata(ctx)
			require.NoError(t, err)
			require.Len(t, all, 1)
			assert.Equal(t, "v2", all[0].FileHash)
			assert.Equal(t, 1, all[0].ChunkCount, "stale chunks from v1 must not remain")

			results, err := db.SearchSimilarVectors(ctx, embeddingsFor(1)[0], 10)
			require.NoError(t, err)
			require.Len(t, results, 1)
			assert.Equal(t, "only", results[0].Content)
		})
	}
}

func TestReplaceFileDocuments_RollsBackOnBadChunkLeavingPreviousVersionIntact(t *testing.T) {
	t.Parallel()
	for _, factory := range vectorDBFactories {
		t.Run(factory.name, func(t *testing.T) {
			t.Parallel()
			db := newTestDB(t, factory)
			ctx := t.Context()

			v1 := docsFor("one", "two")
			require.NoError(t, db.ReplaceFileDocuments(ctx, database.FileMetadata{SourcePath: testDocPath, FileHash: "v1", ChunkCount: 2}, v1, embeddingsFor(2), inputsFor("s", 2)))

			// v2 has a bad final embedding (wrong dimension) - the whole
			// transaction must roll back, leaving v1 exactly as it was.
			v2 := docsFor("new one", "new two")
			badEmbeddings := embeddingsFor(2)
			badEmbeddings[1] = []float64{1, 2} // wrong dimension
			err := db.ReplaceFileDocuments(ctx, database.FileMetadata{SourcePath: testDocPath, FileHash: "v2", ChunkCount: 2}, v2, badEmbeddings, inputsFor("s", 2))
			require.Error(t, err)

			all, err := db.GetAllFileMetadata(ctx)
			require.NoError(t, err)
			require.Len(t, all, 1, "v1's file row must still be present")
			assert.Equal(t, "v1", all[0].FileHash, "hash must not have moved to v2")
			assert.Equal(t, 2, all[0].ChunkCount)

			results, err := db.SearchSimilarVectors(ctx, embeddingsFor(1)[0], 10)
			require.NoError(t, err)
			require.Len(t, results, 2, "v1's chunks must be untouched")
			gotContents := []string{results[0].Content, results[1].Content}
			assert.ElementsMatch(t, []string{"one", "two"}, gotContents, "v1's chunk content must be byte-identical")
		})
	}
}

func TestDeleteDocumentsByPath_CascadesChunksToZero(t *testing.T) {
	t.Parallel()
	for _, factory := range vectorDBFactories {
		t.Run(factory.name, func(t *testing.T) {
			t.Parallel()
			db := newTestDB(t, factory)
			ctx := t.Context()

			docs := docsFor("one", "two")
			require.NoError(t, db.ReplaceFileDocuments(ctx, database.FileMetadata{SourcePath: testDocPath, FileHash: "v1", ChunkCount: 2}, docs, embeddingsFor(2), inputsFor("s", 2)))

			require.NoError(t, db.DeleteDocumentsByPath(ctx, testDocPath))

			all, err := db.GetAllFileMetadata(ctx)
			require.NoError(t, err)
			assert.Empty(t, all)

			results, err := db.SearchSimilarVectors(ctx, embeddingsFor(1)[0], 10)
			require.NoError(t, err)
			assert.Empty(t, results)
		})
	}
}

func TestReplaceFileDocuments_EmbeddingInputRoundTripsOnlyForSemantic(t *testing.T) {
	t.Parallel()
	for _, factory := range vectorDBFactories {
		t.Run(factory.name, func(t *testing.T) {
			t.Parallel()
			db := newTestDB(t, factory)
			ctx := t.Context()

			docs := docsFor("raw chunk content")
			require.NoError(t, db.ReplaceFileDocuments(ctx, database.FileMetadata{SourcePath: testDocPath, FileHash: "v1", ChunkCount: 1}, docs, embeddingsFor(1), []string{"LLM-generated summary"}))

			results, err := db.SearchSimilarVectors(ctx, embeddingsFor(1)[0], 10)
			require.NoError(t, err)
			require.Len(t, results, 1)
			assert.Equal(t, "raw chunk content", results[0].Content)
			if factory.semantic {
				assert.Equal(t, "LLM-generated summary", results[0].EmbeddingInput)
			} else {
				assert.Empty(t, results[0].EmbeddingInput)
			}
		})
	}
}

func TestReplaceFileDocuments_RejectsEmptyDocs(t *testing.T) {
	t.Parallel()
	for _, factory := range vectorDBFactories {
		t.Run(factory.name, func(t *testing.T) {
			t.Parallel()
			db := newTestDB(t, factory)
			ctx := t.Context()

			err := db.ReplaceFileDocuments(ctx, database.FileMetadata{SourcePath: testDocPath, FileHash: "v1", ChunkCount: 0}, nil, nil, nil)
			require.Error(t, err, "replacing with zero documents must be rejected, not silently recreate the zero-chunk-row anomaly this fix targets")

			all, err := db.GetAllFileMetadata(ctx)
			require.NoError(t, err)
			assert.Empty(t, all)
		})
	}
}

func TestGetFileMetadata_ReportsChunkCountOrNil(t *testing.T) {
	t.Parallel()
	for _, factory := range vectorDBFactories {
		t.Run(factory.name, func(t *testing.T) {
			t.Parallel()
			db := newTestDB(t, factory)
			ctx := t.Context()

			meta, err := db.GetFileMetadata(ctx, testDocPath)
			require.NoError(t, err)
			assert.Nil(t, meta, "unknown file yields nil, not an error")

			docs := docsFor("one", "two", "three")
			require.NoError(t, db.ReplaceFileDocuments(ctx, database.FileMetadata{SourcePath: testDocPath, FileHash: "v1", ChunkCount: 3}, docs, embeddingsFor(3), inputsFor("s", 3)))

			meta, err = db.GetFileMetadata(ctx, testDocPath)
			require.NoError(t, err)
			require.NotNil(t, meta)
			assert.Equal(t, testDocPath, meta.SourcePath)
			assert.Equal(t, "v1", meta.FileHash)
			assert.Equal(t, 3, meta.ChunkCount)
			assert.NotEmpty(t, meta.LastIndexed)

			require.NoError(t, db.DeleteFileMetadata(ctx, testDocPath))
			meta, err = db.GetFileMetadata(ctx, testDocPath)
			require.NoError(t, err)
			assert.Nil(t, meta)
		})
	}
}

func TestSearchSimilarVectors_RanksAcrossFilesAndLimits(t *testing.T) {
	t.Parallel()
	for _, factory := range vectorDBFactories {
		t.Run(factory.name, func(t *testing.T) {
			t.Parallel()
			db := newTestDB(t, factory)
			ctx := t.Context()

			for path, embedding := range map[string][]float64{
				"/x.txt": {1, 0, 0},
				"/y.txt": {0, 1, 0},
				"/z.txt": {1, 1, 0},
			} {
				docs := []database.Document{{ID: path, SourcePath: path, ChunkIndex: 0, Content: path, FileHash: "h"}}
				require.NoError(t, db.ReplaceFileDocuments(ctx, database.FileMetadata{SourcePath: path, FileHash: "h", ChunkCount: 1}, docs, [][]float64{embedding}, []string{"s"}))
			}

			results, err := db.SearchSimilarVectors(ctx, []float64{1, 0, 0}, 2)
			require.NoError(t, err)
			require.Len(t, results, 2, "limit is applied after ranking")
			assert.Equal(t, "/x.txt", results[0].SourcePath)
			assert.Equal(t, "/x.txt_0", results[0].ID)
			assert.InDelta(t, 1.0, results[0].Similarity, 1e-9)
			assert.Equal(t, "/z.txt", results[1].SourcePath)
			assert.Equal(t, "h", results[1].FileHash)
			assert.NotEmpty(t, results[1].CreatedAt)
		})
	}
}
