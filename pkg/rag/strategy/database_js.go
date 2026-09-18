//go:build js

package strategy

import (
	"context"
	"log/slog"
)

// sqlite is unavailable under js/wasm, so each constructor returns a fresh
// in-memory store: nothing is shared between strategies or persisted across
// runs, and dbPath only identifies the store in logs.

func newBM25DB(ctx context.Context, dbPath, strategyName string) (*memoryBM25DB, error) {
	slog.InfoContext(ctx, "BM25 database initialized in memory", "path", dbPath, "strategy", strategyName)
	return newMemoryBM25DB(), nil
}

func newChunkedVectorDB(ctx context.Context, dbPath string, vectorDimensions int, strategyName string) (*memoryVectorDB, error) {
	slog.InfoContext(ctx, "Chunked vector database initialized in memory",
		"vector_dimensions", vectorDimensions, "path", dbPath, "strategy", strategyName)
	return newMemoryVectorDB(vectorDimensions, false), nil
}

func newSemanticVectorDB(ctx context.Context, dbPath string, vectorDimensions int, strategyName string) (*memoryVectorDB, error) {
	slog.InfoContext(ctx, "Semantic vector database initialized in memory",
		"vector_dimensions", vectorDimensions, "path", dbPath, "strategy", strategyName)
	return newMemoryVectorDB(vectorDimensions, true), nil
}
