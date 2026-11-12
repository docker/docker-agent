package strategy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"maps"
	"slices"
)

// memorySource serves documents keyed by logical path; nothing touches the
// filesystem. Listing is sorted so indexing order is deterministic.
type memorySource map[string][]byte

func (m memorySource) list(context.Context) ([]string, error) {
	return slices.Sorted(maps.Keys(m)), nil
}

func (m memorySource) hash(path string) (string, error) {
	content, err := m.read(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:]), nil
}

func (m memorySource) read(path string) ([]byte, error) {
	content, ok := m[path]
	if !ok {
		return nil, fmt.Errorf("document %q was not supplied", path)
	}
	return content, nil
}

// InitializeDocuments indexes documents supplied in memory, keyed by logical
// path, exactly like Initialize indexes files.
func (s *BM25Strategy) InitializeDocuments(ctx context.Context, docs map[string][]byte, chunking ChunkingConfig) error {
	slog.InfoContext(ctx, "Starting BM25 strategy initialization from supplied documents",
		"name", s.name,
		"documents", len(docs),
		"chunk_size", chunking.Size,
		"chunk_overlap", chunking.Overlap,
		"respect_word_boundaries", chunking.RespectWordBoundaries)

	return s.initialize(ctx, memorySource(docs), slices.Sorted(maps.Keys(docs)))
}

// InitializeDocuments indexes documents supplied in memory, keyed by logical
// path, exactly like Initialize indexes files.
func (s *VectorStore) InitializeDocuments(ctx context.Context, docs map[string][]byte, chunking ChunkingConfig) error {
	slog.InfoContext(ctx, "Starting vector store initialization from supplied documents",
		"name", s.name,
		"documents", len(docs),
		"chunk_size", chunking.Size,
		"chunk_overlap", chunking.Overlap,
		"respect_word_boundaries", chunking.RespectWordBoundaries,
		"code_aware", chunking.CodeAware)

	return s.initialize(ctx, memorySource(docs), slices.Sorted(maps.Keys(docs)))
}
