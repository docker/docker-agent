package strategy

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/docker/docker-agent/pkg/rag/database"
	"github.com/docker/docker-agent/pkg/sqliteutil"
)

// chunkedVectorDB implements vectorStoreDB for the chunked-embeddings strategy.
// It stores document chunks with their embedding vectors (no semantic summaries).
type chunkedVectorDB struct {
	db               *sql.DB
	ctx              func() context.Context
	vectorDimensions int
	tablePrefix      string
	filesTable       string
	chunksTable      string
}

// newChunkedVectorDB creates a new SQLite database for chunked vector embeddings.
func newChunkedVectorDB(ctx context.Context, dbPath string, vectorDimensions int, strategyName string) (*chunkedVectorDB, error) {
	if err := ensureDir(dbPath); err != nil {
		return nil, fmt.Errorf("failed to create database directory: %w", err)
	}

	db, err := sqliteutil.OpenDB(ctx, dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	tablePrefix := sanitizeTableName(strategyName)

	cdb := &chunkedVectorDB{
		db:               db,
		ctx:              func() context.Context { return context.WithoutCancel(ctx) },
		vectorDimensions: vectorDimensions,
		tablePrefix:      tablePrefix,
		filesTable:       tablePrefix + "_files",
		chunksTable:      tablePrefix + "_chunks",
	}

	if err := cdb.createSchema(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to create schema: %w", err)
	}

	slog.InfoContext(ctx, "Chunked vector database initialized",
		"vector_dimensions", vectorDimensions,
		"path", dbPath,
		"table_prefix", tablePrefix)

	return cdb, nil
}

func (d *chunkedVectorDB) createSchema(ctx context.Context) error {
	schema := fmt.Sprintf( //nolint:gosec // table names are internal, no user input
		`
	CREATE TABLE IF NOT EXISTS %s (
		source_path TEXT PRIMARY KEY,
		file_hash TEXT NOT NULL,
		indexed_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_%s_file_hash ON %s(file_hash);
	
	CREATE TABLE IF NOT EXISTS %s (
		source_path TEXT NOT NULL,
		chunk_index INTEGER NOT NULL,
		content TEXT NOT NULL,
		embedding BLOB NOT NULL,
		PRIMARY KEY (source_path, chunk_index),
		FOREIGN KEY (source_path) REFERENCES %s(source_path) ON DELETE CASCADE
	);
	`, d.filesTable, d.tablePrefix, d.filesTable, d.chunksTable, d.filesTable,
	)

	_, err := d.db.ExecContext(ctx, schema)
	return err
}

// ReplaceFileDocuments implements vectorStoreDB.
// It atomically replaces a file's indexed state in a single transaction:
// delete the existing row (cascades to its chunks) -> insert the files row
// with the final hash -> insert every chunk. If any step fails the whole
// transaction rolls back, leaving the previous version (if any) intact.
// For chunked-embeddings, embeddingInputs is ignored.
func (d *chunkedVectorDB) ReplaceFileDocuments(ctx context.Context, meta database.FileMetadata, docs []database.Document, embeddings [][]float64, embeddingInputs []string) error {
	if len(docs) == 0 {
		return errors.New("replace file documents: at least one document is required; use DeleteDocumentsByPath for an empty file")
	}
	if len(docs) != len(embeddings) || len(docs) != len(embeddingInputs) {
		return fmt.Errorf("replace file documents: got %d docs, %d embeddings, %d embedding inputs", len(docs), len(embeddings), len(embeddingInputs))
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, fmt.Sprintf("DELETE FROM %s WHERE source_path = ?", d.filesTable), meta.SourcePath); err != nil {
		return fmt.Errorf("failed to delete old file row: %w", err)
	}

	_, err = tx.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO %s (source_path, file_hash, indexed_at) VALUES (?, ?, CURRENT_TIMESTAMP)`, d.filesTable),
		meta.SourcePath, meta.FileHash)
	if err != nil {
		return fmt.Errorf("failed to insert file metadata: %w", err)
	}

	chunkStmt, err := tx.PrepareContext(ctx,
		fmt.Sprintf(`INSERT INTO %s (source_path, chunk_index, content, embedding) VALUES (?, ?, ?, ?)`, d.chunksTable))
	if err != nil {
		return fmt.Errorf("failed to prepare chunk insert: %w", err)
	}
	defer chunkStmt.Close()

	for i, doc := range docs {
		embedding := embeddings[i]
		if len(embedding) == 0 {
			return errors.New("embedding is required for vector database")
		}
		if len(embedding) != d.vectorDimensions {
			return fmt.Errorf("embedding dimension mismatch: got %d, expected %d", len(embedding), d.vectorDimensions)
		}

		embJSON, err := json.Marshal(embedding)
		if err != nil {
			return fmt.Errorf("failed to marshal embedding: %w", err)
		}

		if _, err := chunkStmt.ExecContext(ctx, doc.SourcePath, doc.ChunkIndex, doc.Content, embJSON); err != nil {
			return fmt.Errorf("failed to insert chunk %d: %w", doc.ChunkIndex, err)
		}
	}

	return tx.Commit()
}

// SearchSimilarVectors implements vectorStoreDB.
func (d *chunkedVectorDB) SearchSimilarVectors(ctx context.Context, queryEmbedding []float64, limit int) ([]VectorSearchResultData, error) {
	query := fmt.Sprintf( //nolint:gosec // table names are internal, no user input
		`
	SELECT c.source_path, c.chunk_index, c.content, c.embedding, f.file_hash, f.indexed_at
	FROM %s c
	JOIN %s f ON c.source_path = f.source_path
	`, d.chunksTable, d.filesTable,
	)

	rows, err := d.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to query documents: %w", err)
	}
	defer rows.Close()

	var results []VectorSearchResultData
	for rows.Next() {
		var doc database.Document
		var embJSON []byte

		if err := rows.Scan(&doc.SourcePath, &doc.ChunkIndex, &doc.Content,
			&embJSON, &doc.FileHash, &doc.CreatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan row: %w", err)
		}

		doc.ID = fmt.Sprintf("%s_%d", doc.SourcePath, doc.ChunkIndex)

		var embedding []float64
		if err := json.Unmarshal(embJSON, &embedding); err != nil {
			return nil, fmt.Errorf("failed to unmarshal embedding: %w", err)
		}

		similarity := database.CosineSimilarity(queryEmbedding, embedding)
		results = append(results, VectorSearchResultData{
			Document:   doc,
			Embedding:  embedding,
			Similarity: similarity,
		})
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating rows: %w", err)
	}

	slices.SortFunc(results, func(a, b VectorSearchResultData) int {
		return cmp.Compare(b.Similarity, a.Similarity)
	})

	if len(results) > limit {
		results = results[:limit]
	}

	return results, nil
}

func (d *chunkedVectorDB) DeleteDocumentsByPath(ctx context.Context, sourcePath string) error {
	_, err := d.db.ExecContext(ctx, fmt.Sprintf("DELETE FROM %s WHERE source_path = ?", d.filesTable), sourcePath)
	return err
}

func (d *chunkedVectorDB) GetFileMetadata(ctx context.Context, sourcePath string) (*database.FileMetadata, error) {
	var metadata database.FileMetadata

	err := d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT f.source_path, f.file_hash, f.indexed_at, COUNT(c.chunk_index) as chunk_count
		 FROM %s f
		 LEFT JOIN %s c ON f.source_path = c.source_path
		 WHERE f.source_path = ?
		 GROUP BY f.source_path, f.file_hash, f.indexed_at`, d.filesTable, d.chunksTable),
		sourcePath).Scan(&metadata.SourcePath, &metadata.FileHash, &metadata.LastIndexed, &metadata.ChunkCount)

	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get file metadata: %w", err)
	}

	return &metadata, nil
}

func (d *chunkedVectorDB) GetAllFileMetadata(ctx context.Context) ([]database.FileMetadata, error) {
	rows, err := d.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT f.source_path, f.file_hash, f.indexed_at, COUNT(c.chunk_index) as chunk_count
		 FROM %s f
		 LEFT JOIN %s c ON f.source_path = c.source_path
		 GROUP BY f.source_path, f.file_hash, f.indexed_at`, d.filesTable, d.chunksTable))
	if err != nil {
		return nil, fmt.Errorf("failed to query file metadata: %w", err)
	}
	defer rows.Close()

	var metadata []database.FileMetadata
	for rows.Next() {
		var m database.FileMetadata
		if err := rows.Scan(&m.SourcePath, &m.FileHash, &m.LastIndexed, &m.ChunkCount); err != nil {
			return nil, fmt.Errorf("failed to scan metadata row: %w", err)
		}
		metadata = append(metadata, m)
	}

	return metadata, rows.Err()
}

func (d *chunkedVectorDB) DeleteFileMetadata(ctx context.Context, sourcePath string) error {
	_, err := d.db.ExecContext(ctx, fmt.Sprintf("DELETE FROM %s WHERE source_path = ?", d.filesTable), sourcePath)
	return err
}

func (d *chunkedVectorDB) Close() error {
	return sqliteutil.CheckpointAndClose(d.ctx(), d.db)
}
