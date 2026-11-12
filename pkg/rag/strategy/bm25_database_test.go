package strategy

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/rag/database"
)

// bm25DBFactories covers the platform constructor (sqlite natively,
// in-memory under js) and the in-memory backend explicitly.
var bm25DBFactories = []struct {
	name string
	new  func(ctx context.Context, dbPath string) (bm25Database, error)
}{
	{
		name: "bm25",
		new: func(ctx context.Context, dbPath string) (bm25Database, error) {
			return newBM25DB(ctx, dbPath, "bm25")
		},
	},
	{
		name: "memory",
		new: func(context.Context, string) (bm25Database, error) {
			return newMemoryBM25DB(), nil
		},
	},
}

func bm25Doc(id, sourcePath string, chunkIndex int, content, hash string) database.Document {
	return database.Document{ID: id, SourcePath: sourcePath, ChunkIndex: chunkIndex, Content: content, FileHash: hash}
}

func TestBM25Database(t *testing.T) {
	t.Parallel()
	for _, factory := range bm25DBFactories {
		t.Run(factory.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			db, err := factory.new(ctx, filepath.Join(t.TempDir(), "test.db"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })

			t.Run("empty", func(t *testing.T) {
				docs, err := db.GetAllDocuments(ctx)
				require.NoError(t, err)
				assert.Nil(t, docs)

				all, err := db.GetAllFileMetadata(ctx)
				require.NoError(t, err)
				assert.Nil(t, all)

				meta, err := db.GetFileMetadata(ctx, "/missing.txt")
				require.NoError(t, err)
				assert.Nil(t, meta)
			})

			t.Run("documents keep insertion order and upsert by chunk", func(t *testing.T) {
				require.NoError(t, db.AddDocument(ctx, bm25Doc("a0", "/a.txt", 0, "alpha", "h1")))
				require.NoError(t, db.AddDocument(ctx, bm25Doc("b0", "/b.txt", 0, "bravo", "h1")))
				require.NoError(t, db.AddDocument(ctx, bm25Doc("a1", "/a.txt", 1, "alpha two", "h1")))
				require.NoError(t, db.AddDocument(ctx, bm25Doc("a0-new", "/a.txt", 0, "alpha updated", "h2")))

				docs, err := db.GetAllDocuments(ctx)
				require.NoError(t, err)
				require.Len(t, docs, 3)
				assert.Equal(t, []string{"a0", "b0", "a1"}, []string{docs[0].ID, docs[1].ID, docs[2].ID}, "upsert keeps the original ID")
				assert.Equal(t, "alpha updated", docs[0].Content)
				assert.Equal(t, "h2", docs[0].FileHash)
				assert.NotEmpty(t, docs[0].CreatedAt)
			})

			t.Run("metadata upserts and lists sorted", func(t *testing.T) {
				require.NoError(t, db.SetFileMetadata(ctx, database.FileMetadata{SourcePath: "/b.txt", FileHash: "h1", ChunkCount: 1}))
				require.NoError(t, db.SetFileMetadata(ctx, database.FileMetadata{SourcePath: "/a.txt", FileHash: "h1", ChunkCount: 1}))
				require.NoError(t, db.SetFileMetadata(ctx, database.FileMetadata{SourcePath: "/a.txt", FileHash: "h2", ChunkCount: 2}))

				meta, err := db.GetFileMetadata(ctx, "/a.txt")
				require.NoError(t, err)
				require.NotNil(t, meta)
				assert.Equal(t, "h2", meta.FileHash)
				assert.Equal(t, 2, meta.ChunkCount)
				assert.NotEmpty(t, meta.LastIndexed)

				all, err := db.GetAllFileMetadata(ctx)
				require.NoError(t, err)
				require.Len(t, all, 2)
				assert.ElementsMatch(t, []string{"/a.txt", "/b.txt"}, []string{all[0].SourcePath, all[1].SourcePath})
			})

			t.Run("delete by path leaves other files intact", func(t *testing.T) {
				require.NoError(t, db.DeleteDocumentsByPath(ctx, "/a.txt"))
				require.NoError(t, db.DeleteFileMetadata(ctx, "/a.txt"))

				docs, err := db.GetAllDocuments(ctx)
				require.NoError(t, err)
				require.Len(t, docs, 1)
				assert.Equal(t, "/b.txt", docs[0].SourcePath)

				meta, err := db.GetFileMetadata(ctx, "/a.txt")
				require.NoError(t, err)
				assert.Nil(t, meta)

				all, err := db.GetAllFileMetadata(ctx)
				require.NoError(t, err)
				require.Len(t, all, 1)
				assert.Equal(t, "/b.txt", all[0].SourcePath)
			})
		})
	}
}
