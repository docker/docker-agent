package strategy

import (
	"cmp"
	"context"
	"fmt"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/rag/types"
)

// NewChunkedEmbeddingsFromConfig creates a chunked-embeddings strategy from configuration.
//
// This strategy embeds document chunks directly and uses vector similarity search
// for retrieval. It's the simplest embedding-based RAG strategy.
func NewChunkedEmbeddingsFromConfig(ctx context.Context, cfg latest.RAGStrategyConfig, buildCtx BuildContext, events chan<- types.Event) (*Config, error) {
	const strategyName = "chunked-embeddings"

	// Extract required parameters
	modelName := GetParam(cfg.Params, "embedding_model", "")
	if modelName == "" {
		return nil, fmt.Errorf("'embedding_model' parameter required for %s strategy", strategyName)
	}

	// vector_dimensions is required because embedding dimensionality depends on
	// the chosen model, and using an incorrect default could corrupt the database.
	vectorDimensionsPtr := GetParamPtr[int](cfg.Params, "vector_dimensions")
	if vectorDimensionsPtr == nil {
		return nil, fmt.Errorf("'vector_dimensions' parameter required for %s strategy", strategyName)
	}
	vectorDimensions := *vectorDimensionsPtr

	// Create embedding provider
	embeddingCfg, err := CreateEmbeddingProvider(ctx, modelName, buildCtx)
	if err != nil {
		return nil, err
	}

	// Get optional parameters with defaults
	similarityMetric := GetParam(cfg.Params, "similarity_metric", "cosine_similarity")
	threshold := GetParam(cfg.Params, "threshold", 0.5)
	if thresholdPtr := GetParamPtr[float64](cfg.Params, "threshold"); thresholdPtr != nil {
		threshold = *thresholdPtr
	}

	batchSize := GetParam(cfg.Params, "embedding_batch_size", 50)
	maxConcurrency := GetParam(cfg.Params, "max_embedding_concurrency", 3)
	fileIndexConcurrency := GetParam(cfg.Params, "max_indexing_concurrency", 3)

	// Merge document paths
	docs := MergeDocPaths(buildCtx.SharedDocs, cfg.Docs, buildCtx.ParentDir)

	// Resolve database path
	dbPath, err := ResolveDatabasePath(cfg.Database, buildCtx.ParentDir,
		fmt.Sprintf("rag_%s_chunked_embeddings.db", buildCtx.RAGName))
	if err != nil {
		return nil, fmt.Errorf("invalid database config: %w", err)
	}

	// Create vector database (internal to this strategy)
	db, err := newChunkedVectorDB(dbPath, vectorDimensions, strategyName)
	if err != nil {
		return nil, fmt.Errorf("failed to create database: %w", err)
	}

	// Create embedder
	embedder := CreateEmbedder(embeddingCfg.Provider, batchSize, maxConcurrency)

	// Set default limit if not provided
	limit := cmp.Or(cfg.Limit, 5)

	// Parse chunking configuration
	chunkingCfg := ParseChunkingConfig(cfg)

	// Create vector store
	store := NewVectorStore(VectorStoreConfig{
		Name:                 strategyName,
		Database:             db,
		Embedder:             embedder,
		Events:               events,
		SimilarityMetric:     similarityMetric,
		ModelID:              embeddingCfg.ModelID,
		ModelsStore:          embeddingCfg.ModelsStore,
		EmbeddingConcurrency: maxConcurrency,
		FileIndexConcurrency: fileIndexConcurrency,
		Chunking:             chunkingCfg,
		ShouldIgnore:         BuildShouldIgnore(buildCtx, cfg.Params),
	})

	return &Config{
		Name:      strategyName,
		Strategy:  store,
		Docs:      docs,
		Limit:     limit,
		Threshold: threshold,
		Chunking:  chunkingCfg,
	}, nil
}
