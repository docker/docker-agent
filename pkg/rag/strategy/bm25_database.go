package strategy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/docker/docker-agent/pkg/rag/database"
	"github.com/docker/docker-agent/pkg/sqliteutil"
)

// bm25DB implements the database for BM25 strategy (no vectors needed).
// Uses the base Document type without embeddings.
type bm25DB struct {
	db            *sql.DB
	tablePrefix   string
	docsTable     string
	metadataTable string
}

// newBM25DB creates a new SQLite database for BM25 strategy.
func newBM25DB(dbPath, strategyName string) (*bm25DB, error) {
	if err := ensureDir(dbPath); err != nil {
		return nil, fmt.Errorf("failed to create database directory: %w", err)
	}

	db, err := sqliteutil.OpenDB(dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	tablePrefix := sanitizeTableName(strategyName)

	bdb := &bm25DB{
		db:            db,
		tablePrefix:   tablePrefix,
		docsTable:     tablePrefix + "_documents",
		metadataTable: tablePrefix + "_file_metadata",
	}

	if err := bdb.createSchema(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to create schema: %w", err)
	}

	slog.Info("BM25 database initialized", "path", dbPath, "table_prefix", tablePrefix)

	return bdb, nil
}

func (d *bm25DB) createSchema() error {
	schema := fmt.Sprintf(`
	CREATE TABLE IF NOT EXISTS %s (
		id TEXT PRIMARY KEY,
		source_path TEXT NOT NULL,
		chunk_index INTEGER NOT NULL,
		content TEXT NOT NULL,
		file_hash TEXT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(source_path, chunk_index)
	);
	CREATE INDEX IF NOT EXISTS idx_%s_source_path ON %s(source_path);
	CREATE INDEX IF NOT EXISTS idx_%s_file_hash ON %s(file_hash);
	CREATE INDEX IF NOT EXISTS idx_%s_content_fts ON %s(content);
	
	CREATE TABLE IF NOT EXISTS %s (
		source_path TEXT PRIMARY KEY,
		file_hash TEXT NOT NULL,
		last_indexed DATETIME DEFAULT CURRENT_TIMESTAMP,
		chunk_count INTEGER DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS idx_%s_metadata_hash ON %s(file_hash);
	`, d.docsTable,
		d.tablePrefix, d.docsTable,
		d.tablePrefix, d.docsTable,
		d.tablePrefix, d.docsTable,
		d.metadataTable,
		d.tablePrefix, d.metadataTable)

	_, err := d.db.ExecContext(context.Background(), schema)
	return err
}

func (d *bm25DB) AddDocument(ctx context.Context, doc database.Document) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var exists bool
	err = tx.QueryRowContext(ctx,
		fmt.Sprintf("SELECT 1 FROM %s WHERE source_path = ? AND chunk_index = ?", d.docsTable),
		doc.SourcePath, doc.ChunkIndex).Scan(&exists)

	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("failed to check existing document: %w", err)
	}

	if exists {
		_, err = tx.ExecContext(ctx,
			fmt.Sprintf(`UPDATE %s SET content = ?, file_hash = ?, created_at = CURRENT_TIMESTAMP 
			 WHERE source_path = ? AND chunk_index = ?`, d.docsTable),
			doc.Content, doc.FileHash, doc.SourcePath, doc.ChunkIndex)
		if err != nil {
			return fmt.Errorf("failed to update document: %w", err)
		}
	} else {
		_, err = tx.ExecContext(ctx,
			fmt.Sprintf(`INSERT INTO %s (id, source_path, chunk_index, content, file_hash)
			 VALUES (?, ?, ?, ?, ?)`, d.docsTable),
			doc.ID, doc.SourcePath, doc.ChunkIndex, doc.Content, doc.FileHash)
		if err != nil {
			return fmt.Errorf("failed to insert document: %w", err)
		}
	}

	return tx.Commit()
}

func (d *bm25DB) DeleteDocumentsByPath(ctx context.Context, sourcePath string) error {
	_, err := d.db.ExecContext(ctx, fmt.Sprintf("DELETE FROM %s WHERE source_path = ?", d.docsTable), sourcePath)
	return err
}

func (d *bm25DB) GetAllDocuments(ctx context.Context) ([]database.Document, error) {
	query := fmt.Sprintf( //nolint:gosec // table name is internal, no user input
		`
	SELECT id, source_path, chunk_index, content, file_hash, created_at
	FROM %s
	`, d.docsTable)

	rows, err := d.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to query documents: %w", err)
	}
	defer rows.Close()

	var docs []database.Document
	for rows.Next() {
		var doc database.Document
		if err := rows.Scan(&doc.ID, &doc.SourcePath, &doc.ChunkIndex, &doc.Content,
			&doc.FileHash, &doc.CreatedAt); err != nil {
			return nil, fmt.Errorf("failed to scan row: %w", err)
		}
		docs = append(docs, doc)
	}

	return docs, rows.Err()
}

func (d *bm25DB) GetFileMetadata(ctx context.Context, sourcePath string) (*database.FileMetadata, error) {
	var metadata database.FileMetadata
	err := d.db.QueryRowContext(ctx,
		fmt.Sprintf("SELECT source_path, file_hash, last_indexed, chunk_count FROM %s WHERE source_path = ?", d.metadataTable),
		sourcePath).Scan(&metadata.SourcePath, &metadata.FileHash, &metadata.LastIndexed, &metadata.ChunkCount)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get file metadata: %w", err)
	}

	return &metadata, nil
}

func (d *bm25DB) SetFileMetadata(ctx context.Context, metadata database.FileMetadata) error {
	query := fmt.Sprintf( //nolint:gosec // table name is internal, values are bound parameters
		`
	INSERT INTO %s (source_path, file_hash, last_indexed, chunk_count)
	VALUES (?, ?, CURRENT_TIMESTAMP, ?)
	ON CONFLICT(source_path) DO UPDATE SET
		file_hash = excluded.file_hash,
		last_indexed = CURRENT_TIMESTAMP,
		chunk_count = excluded.chunk_count
	`, d.metadataTable)

	_, err := d.db.ExecContext(ctx, query, metadata.SourcePath, metadata.FileHash, metadata.ChunkCount)
	return err
}

func (d *bm25DB) GetAllFileMetadata(ctx context.Context) ([]database.FileMetadata, error) {
	rows, err := d.db.QueryContext(ctx,
		"SELECT source_path, file_hash, last_indexed, chunk_count FROM "+d.metadataTable) //nolint:gosec // table name is internal, no user input
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

func (d *bm25DB) DeleteFileMetadata(ctx context.Context, sourcePath string) error {
	_, err := d.db.ExecContext(ctx, fmt.Sprintf("DELETE FROM %s WHERE source_path = ?", d.metadataTable), sourcePath)
	return err
}

func (d *bm25DB) Close() error {
	return sqliteutil.CheckpointAndClose(d.db)
}

// ensureDir creates the parent directory for a file path if it doesn't exist
func ensureDir(filePath string) error {
	dir := filepath.Dir(filePath)
	if dir == "" || dir == "." {
		return nil
	}

	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return os.MkdirAll(dir, 0o700)
	}

	return nil
}

// sanitizeTableName converts a strategy name into a valid SQLite table name prefix.
func sanitizeTableName(name string) string {
	result := make([]byte, 0, len(name))
	for i := range len(name) {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_':
			result = append(result, c)
		case c == '-':
			result = append(result, '_')
		}
	}
	if len(result) == 0 {
		return "default"
	}
	return string(result)
}
