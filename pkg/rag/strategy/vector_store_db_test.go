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
// below, so they can run identically against both the semantic-embeddings
// and chunked-embeddings SQLite implementations.
type vectorDBFactory struct {
	name string
	new  func(ctx context.Context, dbPath string) (vectorStoreDB, error)
}

var vectorDBFactories = []vectorDBFactory{
	{
		name: "semantic",
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

func TestReplaceFileDocuments_SemanticEmbeddingInputRoundTrips(t *testing.T) {
	t.Parallel()
	db := newTestDB(t, vectorDBFactories[0]) // semantic
	ctx := t.Context()

	docs := docsFor("raw chunk content")
	require.NoError(t, db.ReplaceFileDocuments(ctx, database.FileMetadata{SourcePath: testDocPath, FileHash: "v1", ChunkCount: 1}, docs, embeddingsFor(1), []string{"LLM-generated summary"}))

	results, err := db.SearchSimilarVectors(ctx, embeddingsFor(1)[0], 10)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "LLM-generated summary", results[0].EmbeddingInput)
	assert.Equal(t, "raw chunk content", results[0].Content)
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
