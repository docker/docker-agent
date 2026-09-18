package strategy

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/rag/database"
)

// In-memory backends for platforms without sqlite (js/wasm). They follow the
// sqlite implementations' observable behavior - insertion order, nil results
// when empty, atomic file replacement - but nothing is persisted: contents
// are lost when the strategy is closed or the process exits.

// memoryTimestamp matches sqlite's CURRENT_TIMESTAMP format so CreatedAt and
// LastIndexed look the same on every backend.
func memoryTimestamp() string {
	return time.Now().UTC().Format(time.DateTime)
}

type memoryBM25DB struct {
	mu       sync.RWMutex
	docs     []database.Document // insertion order, like sqlite rowid order
	metadata map[string]database.FileMetadata
}

var _ bm25Database = (*memoryBM25DB)(nil)

func newMemoryBM25DB() *memoryBM25DB {
	return &memoryBM25DB{metadata: make(map[string]database.FileMetadata)}
}

// AddDocument inserts doc or, when a chunk with the same (source_path,
// chunk_index) exists, updates it in place keeping the original ID.
func (d *memoryBM25DB) AddDocument(ctx context.Context, doc database.Document) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	doc.CreatedAt = memoryTimestamp()
	i := slices.IndexFunc(d.docs, func(existing database.Document) bool {
		return existing.SourcePath == doc.SourcePath && existing.ChunkIndex == doc.ChunkIndex
	})
	if i < 0 {
		d.docs = append(d.docs, doc)
		return nil
	}
	doc.ID = d.docs[i].ID
	d.docs[i] = doc
	return nil
}

func (d *memoryBM25DB) DeleteDocumentsByPath(ctx context.Context, sourcePath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	d.docs = slices.DeleteFunc(d.docs, func(doc database.Document) bool {
		return doc.SourcePath == sourcePath
	})
	return nil
}

func (d *memoryBM25DB) GetAllDocuments(ctx context.Context) ([]database.Document, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	d.mu.RLock()
	defer d.mu.RUnlock()

	if len(d.docs) == 0 {
		return nil, nil
	}
	return slices.Clone(d.docs), nil
}

func (d *memoryBM25DB) GetFileMetadata(ctx context.Context, sourcePath string) (*database.FileMetadata, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	d.mu.RLock()
	defer d.mu.RUnlock()

	metadata, ok := d.metadata[sourcePath]
	if !ok {
		return nil, nil
	}
	return &metadata, nil
}

func (d *memoryBM25DB) SetFileMetadata(ctx context.Context, metadata database.FileMetadata) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	metadata.LastIndexed = memoryTimestamp()
	d.metadata[metadata.SourcePath] = metadata
	return nil
}

func (d *memoryBM25DB) GetAllFileMetadata(ctx context.Context) ([]database.FileMetadata, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	d.mu.RLock()
	defer d.mu.RUnlock()

	var all []database.FileMetadata
	for _, metadata := range d.metadata {
		all = append(all, metadata)
	}
	slices.SortFunc(all, func(a, b database.FileMetadata) int {
		return cmp.Compare(a.SourcePath, b.SourcePath)
	})
	return all, nil
}

func (d *memoryBM25DB) DeleteFileMetadata(ctx context.Context, sourcePath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	delete(d.metadata, sourcePath)
	return nil
}

func (d *memoryBM25DB) Close() error {
	return nil
}

type memoryVectorDB struct {
	vectorDimensions   int
	keepEmbeddingInput bool // semantic-embeddings records what text was embedded; chunked-embeddings does not

	mu    sync.RWMutex
	files map[string]memoryFile
}

type memoryFile struct {
	hash      string
	indexedAt string
	chunks    []memoryChunk
}

type memoryChunk struct {
	index          int
	content        string
	embedding      []float64
	embeddingInput string
}

var _ vectorStoreDB = (*memoryVectorDB)(nil)

func newMemoryVectorDB(vectorDimensions int, keepEmbeddingInput bool) *memoryVectorDB {
	return &memoryVectorDB{
		vectorDimensions:   vectorDimensions,
		keepEmbeddingInput: keepEmbeddingInput,
		files:              make(map[string]memoryFile),
	}
}

// ReplaceFileDocuments implements vectorStoreDB. Every chunk is validated
// before the file is swapped in, so a bad embedding leaves the previous
// version intact, like the sqlite transaction rollback.
func (d *memoryVectorDB) ReplaceFileDocuments(ctx context.Context, meta database.FileMetadata, docs []database.Document, embeddings [][]float64, embeddingInputs []string) error {
	if len(docs) == 0 {
		return errors.New("replace file documents: at least one document is required; use DeleteDocumentsByPath for an empty file")
	}
	if len(docs) != len(embeddings) || len(docs) != len(embeddingInputs) {
		return fmt.Errorf("replace file documents: got %d docs, %d embeddings, %d embedding inputs", len(docs), len(embeddings), len(embeddingInputs))
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	chunks := make([]memoryChunk, len(docs))
	for i, doc := range docs {
		embedding := embeddings[i]
		if len(embedding) == 0 {
			return errors.New("embedding is required for vector database")
		}
		if len(embedding) != d.vectorDimensions {
			return fmt.Errorf("embedding dimension mismatch: got %d, expected %d", len(embedding), d.vectorDimensions)
		}

		chunks[i] = memoryChunk{
			index:     doc.ChunkIndex,
			content:   doc.Content,
			embedding: slices.Clone(embedding),
		}
		if d.keepEmbeddingInput {
			chunks[i].embeddingInput = embeddingInputs[i]
		}
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	d.files[meta.SourcePath] = memoryFile{
		hash:      meta.FileHash,
		indexedAt: memoryTimestamp(),
		chunks:    chunks,
	}
	return nil
}

// SearchSimilarVectors implements vectorStoreDB.
func (d *memoryVectorDB) SearchSimilarVectors(ctx context.Context, queryEmbedding []float64, limit int) ([]VectorSearchResultData, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	d.mu.RLock()
	defer d.mu.RUnlock()

	var results []VectorSearchResultData
	for sourcePath, file := range d.files {
		for _, ch := range file.chunks {
			results = append(results, VectorSearchResultData{
				Document: database.Document{
					ID:         fmt.Sprintf("%s_%d", sourcePath, ch.index),
					SourcePath: sourcePath,
					ChunkIndex: ch.index,
					Content:    ch.content,
					FileHash:   file.hash,
					CreatedAt:  file.indexedAt,
				},
				Embedding:      ch.embedding,
				EmbeddingInput: ch.embeddingInput,
				Similarity:     database.CosineSimilarity(queryEmbedding, ch.embedding),
			})
		}
	}

	// Map iteration is random; break similarity ties by position so results are stable.
	slices.SortFunc(results, func(a, b VectorSearchResultData) int {
		return cmp.Or(
			cmp.Compare(b.Similarity, a.Similarity),
			cmp.Compare(a.SourcePath, b.SourcePath),
			cmp.Compare(a.ChunkIndex, b.ChunkIndex),
		)
	})

	if len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

func (d *memoryVectorDB) DeleteDocumentsByPath(ctx context.Context, sourcePath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	delete(d.files, sourcePath)
	return nil
}

func (d *memoryVectorDB) GetFileMetadata(ctx context.Context, sourcePath string) (*database.FileMetadata, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	d.mu.RLock()
	defer d.mu.RUnlock()

	file, ok := d.files[sourcePath]
	if !ok {
		return nil, nil
	}
	metadata := file.metadata(sourcePath)
	return &metadata, nil
}

func (d *memoryVectorDB) GetAllFileMetadata(ctx context.Context) ([]database.FileMetadata, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	d.mu.RLock()
	defer d.mu.RUnlock()

	var all []database.FileMetadata
	for sourcePath, file := range d.files {
		all = append(all, file.metadata(sourcePath))
	}
	slices.SortFunc(all, func(a, b database.FileMetadata) int {
		return cmp.Compare(a.SourcePath, b.SourcePath)
	})
	return all, nil
}

// DeleteFileMetadata drops the file and its chunks, matching the sqlite
// schema where chunks cascade from the files row.
func (d *memoryVectorDB) DeleteFileMetadata(ctx context.Context, sourcePath string) error {
	return d.DeleteDocumentsByPath(ctx, sourcePath)
}

func (d *memoryVectorDB) Close() error {
	return nil
}

func (f memoryFile) metadata(sourcePath string) database.FileMetadata {
	return database.FileMetadata{
		SourcePath:  sourcePath,
		FileHash:    f.hash,
		LastIndexed: f.indexedAt,
		ChunkCount:  len(f.chunks),
	}
}
