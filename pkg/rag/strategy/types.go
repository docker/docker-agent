package strategy

import (
	"context"

	"github.com/docker/docker-agent/pkg/rag/database"
)

// Strategy defines the interface for different retrieval strategies.
// This is the canonical definition used by both the strategies and rag packages.
type Strategy interface {
	// Initialize indexes all documents from the given paths.
	Initialize(ctx context.Context, docPaths []string, chunking ChunkingConfig) error

	// Query searches for relevant documents using the strategy's retrieval method.
	// numResults is the maximum number of candidates to retrieve (before fusion).
	Query(ctx context.Context, query string, numResults int, threshold float64) ([]database.SearchResult, error)

	// CheckAndReindexChangedFiles checks for file changes and re-indexes if needed.
	CheckAndReindexChangedFiles(ctx context.Context, docPaths []string, chunking ChunkingConfig) error

	// StartFileWatcher starts monitoring files for changes.
	StartFileWatcher(ctx context.Context, docPaths []string, chunking ChunkingConfig) error

	// Close releases resources held by the strategy.
	Close() error
}

// Config contains a strategy and its runtime configuration.
type Config struct {
	Name      string
	Strategy  Strategy
	Docs      []string // Merged document paths (shared + strategy-specific)
	Limit     int      // Max results for this strategy
	Threshold float64  // Score threshold
	Chunking  ChunkingConfig
}

// ChunkingConfig holds chunking parameters.
type ChunkingConfig struct {
	Size                  int
	Overlap               int
	RespectWordBoundaries bool
	CodeAware             bool
}
