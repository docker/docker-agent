package rag

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/docker/docker-agent/pkg/rag/database"
	"github.com/docker/docker-agent/pkg/rag/fusion"
	"github.com/docker/docker-agent/pkg/rag/rerank"
	"github.com/docker/docker-agent/pkg/rag/strategy"
	"github.com/docker/docker-agent/pkg/rag/types"
)

// ToolConfig represents tool-specific configuration
type ToolConfig struct {
	Name        string
	Description string
	Instruction string
}

// Config represents RAG manager configuration in domain terms,
// independent of any particular config schema version.
type Config struct {
	Tool            ToolConfig
	Docs            []string
	Results         ResultsConfig
	FusionConfig    *FusionConfig
	StrategyConfigs []strategy.Config
}

// ResultsConfig captures result-postprocessing behavior for the manager.
type ResultsConfig struct {
	Limit             int              // Maximum number of results to return (top K)
	Deduplicate       bool             // Remove duplicate entries based on final content
	IncludeScore      bool             // Include relevance scores in results (if/when used)
	ReturnFullContent bool             // Return full document content instead of just matched chunks
	RerankingConfig   *RerankingConfig // Optional reranking configuration
}

// RerankingConfig holds configuration for result reranking
type RerankingConfig struct {
	Reranker  rerank.Reranker // The reranker instance (already configured)
	TopK      int             // Optional: only rerank top K results (0 = rerank all)
	Threshold float64         // Optional: minimum score threshold after reranking
}

// Manager orchestrates RAG operations using pluggable strategies
// Supports both single-strategy and hybrid multi-strategy retrieval with fusion
type Manager struct {
	name            string
	config          Config
	strategies      map[string]strategy.Strategy // Map of strategy name -> strategy instance
	strategyConfigs map[string]strategy.Config   // Store configs for per-strategy operations
	fusion          fusion.Fusion                // Fusion strategy for combining multi-strategy results
	reranker        rerank.Reranker              // Optional reranker for result re-scoring
	events          <-chan types.Event           // Shared event channel from strategies and other RAG operations
}

// FusionConfig holds configuration for result fusion
type FusionConfig struct {
	Strategy string             // "rrf", "weighted", "max"
	K        int                // RRF parameter
	Weights  map[string]float64 // Strategy weights
}

// New creates a new RAG manager with one or more strategies.
// Pass multiple strategy configs to enable hybrid retrieval.
// The strategyEvents channel should be shared across all strategies for this manager.
func New(_ context.Context, name string, config Config, strategyEvents <-chan types.Event) (*Manager, error) {
	if len(config.StrategyConfigs) == 0 {
		return nil, errors.New("at least one strategy required")
	}

	// Build strategies map from configs
	strategyMap := make(map[string]strategy.Strategy)
	for _, strategyCfg := range config.StrategyConfigs {
		strategyMap[strategyCfg.Name] = strategyCfg.Strategy
	}

	// Create fusion strategy if multiple strategies
	var fusionStrategy fusion.Fusion
	if len(config.StrategyConfigs) > 1 {
		fusionConfig := config.FusionConfig
		if fusionConfig == nil {
			// Default to RRF
			fusionConfig = &FusionConfig{Strategy: "rrf"}
		}

		fusionCfg := fusion.Config{
			Strategy: fusionConfig.Strategy,
			K:        fusionConfig.K,
			Weights:  fusionConfig.Weights,
		}

		var err error
		fusionStrategy, err = fusion.New(fusionCfg)
		if err != nil {
			return nil, fmt.Errorf("failed to create fusion strategy: %w", err)
		}

		// Ensure fusion was actually created
		if fusionStrategy == nil {
			return nil, errors.New("fusion strategy is nil after creation (this is a bug)")
		}
	}

	// Store strategy configs for later use
	strategyConfigMap := make(map[string]strategy.Config)
	for _, sc := range config.StrategyConfigs {
		strategyConfigMap[sc.Name] = sc
	}

	// Extract reranker if configured
	var reranker rerank.Reranker
	if config.Results.RerankingConfig != nil {
		reranker = config.Results.RerankingConfig.Reranker
		slog.Debug("[RAG Manager] Reranking enabled",
			"rag_name", name,
			"top_k", config.Results.RerankingConfig.TopK,
			"threshold", config.Results.RerankingConfig.Threshold)
	}

	m := &Manager{
		name:            name,
		config:          config,
		strategies:      strategyMap,
		strategyConfigs: strategyConfigMap,
		fusion:          fusionStrategy,
		reranker:        reranker,
		events:          strategyEvents,
	}

	return m, nil
}

// Initialize indexes all documents using all configured strategies
// Each strategy indexes its own document set (shared + strategy-specific)
// Strategies are initialized in parallel for better performance
func (m *Manager) Initialize(ctx context.Context) error {
	slog.DebugContext(ctx, "[RAG Manager] Starting initialization",
		"rag_name", m.name,
		"num_strategies", len(m.strategies))

	// Initialize strategies in parallel to avoid blocking
	type result struct {
		strategyName string
		err          error
	}

	resultsChan := make(chan result, len(m.strategies))

	for strategyName, strategyImpl := range m.strategies {
		strategyCfg := m.strategyConfigs[strategyName]

		go func() {
			slog.DebugContext(ctx, "[RAG Manager] Initializing strategy",
				"rag_name", m.name,
				"strategy", strategyName,
				"num_docs", len(strategyCfg.Docs),
				"chunk_size", strategyCfg.Chunking.Size,
				"chunk_overlap", strategyCfg.Chunking.Overlap,
				"respect_word_boundaries", strategyCfg.Chunking.RespectWordBoundaries,
				"code_aware", strategyCfg.Chunking.CodeAware)

			start := time.Now()
			err := strategyImpl.Initialize(ctx, strategyCfg.Docs, strategyCfg.Chunking)
			indexDuration := time.Since(start)
			slog.DebugContext(ctx, "[RAG Manager] Strategy indexing duration",
				"rag_name", m.name,
				"strategy", strategyName,
				"duration", indexDuration)
			if err != nil {
				slog.ErrorContext(ctx, "[RAG Manager] Strategy initialization failed",
					"rag_name", m.name,
					"strategy", strategyName,
					"error", err)
			} else {
				slog.InfoContext(ctx, "[RAG Manager] Strategy initialized successfully",
					"rag_name", m.name,
					"strategy", strategyName)
			}
			resultsChan <- result{strategyName: strategyName, err: err}
		}()
	}

	// Wait for all strategies to complete
	var firstError error
	for range len(m.strategies) {
		res := <-resultsChan
		if res.err != nil && firstError == nil {
			firstError = res.err
		}
	}

	if firstError != nil {
		return fmt.Errorf("one or more strategies failed to initialize: %w", firstError)
	}

	slog.InfoContext(ctx, "[RAG Manager] Initialization complete",
		"rag_name", m.name)

	return nil
}

// Query searches for relevant documents using all configured strategies
// If multiple strategies are configured, results are combined using the fusion strategy
func (m *Manager) Query(ctx context.Context, query string) ([]database.SearchResult, error) {
	slog.DebugContext(ctx, "[RAG Manager] Starting query",
		"rag_name", m.name,
		"num_strategies", len(m.strategies),
		"query_length", len(query))

	// Single retrieval strategy
	if len(m.strategies) == 1 {
		for strategyName, strategyImpl := range m.strategies {
			strategyCfg := m.strategyConfigs[strategyName]

			slog.DebugContext(ctx, "[RAG Manager] Single strategy query",
				"rag_name", m.name,
				"strategy", strategyName,
				"strategy_limit", strategyCfg.Limit,
				"strategy_threshold", strategyCfg.Threshold)

			results, err := strategyImpl.Query(ctx, query, strategyCfg.Limit, strategyCfg.Threshold)
			if err != nil {
				slog.ErrorContext(ctx, "[RAG Manager] Strategy query failed",
					"rag_name", m.name,
					"strategy", strategyName,
					"error", err)
				return nil, err
			}

			slog.DebugContext(ctx, "[RAG Manager] Single strategy results",
				"rag_name", m.name,
				"strategy", strategyName,
				"num_results", len(results))

			// Apply reranking if configured
			if m.reranker != nil {
				beforeCount := len(results)
				slog.DebugContext(ctx, "[RAG Manager] Applying reranking to single-strategy results",
					"rag_name", m.name,
					"strategy", strategyName,
					"result_count_before", beforeCount)

				rerankedResults, rerankErr := m.reranker.Rerank(ctx, query, results)
				if rerankErr != nil {
					slog.WarnContext(ctx, "[RAG Manager] Reranking failed, using original results",
						"rag_name", m.name,
						"strategy", strategyName,
						"error", rerankErr)
					// Continue with original results rather than failing completely
				} else {
					results = rerankedResults
					slog.DebugContext(ctx, "[RAG Manager] Reranked single-strategy results",
						"rag_name", m.name,
						"strategy", strategyName,
						"result_count_before", beforeCount,
						"result_count_after", len(results),
						"filtered", beforeCount-len(results))
				}
			}

			if limit := m.config.Results.Limit; limit > 0 && len(results) > limit {
				slog.DebugContext(ctx, "[RAG Manager] Truncating to global result limit",
					"rag_name", m.name,
					"strategy", strategyName,
					"before", len(results),
					"after", limit)
				results = results[:limit]
			}

			// Reconstruct full documents if configured
			if m.config.Results.ReturnFullContent {
				results = m.reconstructFullDocuments(ctx, results)
			}

			if m.config.Results.Deduplicate {
				results = m.deduplicateResults(results)
				slog.DebugContext(ctx, "[RAG Manager] Deduplicated single-strategy results",
					"rag_name", m.name,
					"strategy", strategyName,
					"num_results", len(results))
			}

			return results, nil
		}
	}

	// Multi-strategy - query all in parallel with per-strategy limits, then fuse results
	slog.DebugContext(ctx, "[RAG Manager] Multi-strategy query (hybrid)",
		"rag_name", m.name,
		"strategies", getStrategyNames(m.strategies))

	type strategyResult struct {
		name    string
		results []database.SearchResult
		err     error
	}

	resultsChan := make(chan strategyResult, len(m.strategies))

	// Launch parallel queries based on the available retrieval strategies
	for strategyName, strategyImpl := range m.strategies {
		strategyCfg := m.strategyConfigs[strategyName]

		slog.DebugContext(ctx, "[RAG Manager] Launching parallel query for strategy",
			"rag_name", m.name,
			"strategy", strategyName,
			"strategy_limit", strategyCfg.Limit,
			"strategy_threshold", strategyCfg.Threshold)

		go func(name string, strategyImpl strategy.Strategy, cfg strategy.Config) {
			results, err := strategyImpl.Query(ctx, query, cfg.Limit, cfg.Threshold)
			resultsChan <- strategyResult{
				name:    name,
				results: results,
				err:     err,
			}
		}(strategyName, strategyImpl, strategyCfg)
	}

	// Collect results from all strategies
	strategyResults := make(map[string][]database.SearchResult)
	for range len(m.strategies) {
		result := <-resultsChan

		if result.err != nil {
			slog.ErrorContext(ctx, "[RAG Manager] Strategy query failed",
				"rag_name", m.name,
				"strategy", result.name,
				"error", result.err)
			return nil, fmt.Errorf("strategy %s failed: %w", result.name, result.err)
		}

		slog.DebugContext(ctx, "[RAG Manager] Strategy returned results",
			"rag_name", m.name,
			"strategy", result.name,
			"num_results", len(result.results),
			"limit_was", m.strategyConfigs[result.name].Limit)

		strategyResults[result.name] = result.results
	}

	// Fuse results from all strategies
	slog.DebugContext(ctx, "[RAG Manager] Starting fusion",
		"rag_name", m.name,
		"num_strategies", len(strategyResults))

	// Safety check: fusion should never be nil with multiple strategies
	if m.fusion == nil {
		return nil, errors.New("fusion strategy is nil but multiple strategies are configured (this is a bug)")
	}

	fusedResults, err := m.fusion.Fuse(strategyResults)
	if err != nil {
		slog.ErrorContext(ctx, "[RAG Manager] Fusion failed",
			"rag_name", m.name,
			"error", err)
		return nil, fmt.Errorf("failed to fuse results: %w", err)
	}

	slog.DebugContext(ctx, "[RAG Manager] Fusion complete",
		"rag_name", m.name,
		"fused_results", len(fusedResults),
		"result_limit", m.config.Results.Limit)

	// Apply reranking if configured (before limit and deduplication)
	if m.reranker != nil {
		beforeCount := len(fusedResults)
		slog.DebugContext(ctx, "[RAG Manager] Applying reranking to fused results",
			"rag_name", m.name,
			"result_count_before", beforeCount)

		rerankedResults, rerankErr := m.reranker.Rerank(ctx, query, fusedResults)
		if rerankErr != nil {
			slog.WarnContext(ctx, "[RAG Manager] Reranking failed, using original fused results",
				"rag_name", m.name,
				"error", rerankErr)
			// Continue with original fused results rather than failing completely
		} else {
			fusedResults = rerankedResults
			slog.DebugContext(ctx, "[RAG Manager] Reranked fused results",
				"rag_name", m.name,
				"result_count_before", beforeCount,
				"result_count_after", len(fusedResults),
				"filtered", beforeCount-len(fusedResults))
		}
	}

	// Apply result limit if configured
	if limit := m.config.Results.Limit; limit > 0 && len(fusedResults) > limit {
		slog.DebugContext(ctx, "[RAG Manager] Truncating to result limit",
			"rag_name", m.name,
			"before", len(fusedResults),
			"after", limit)
		fusedResults = fusedResults[:limit]
	}

	// Reconstruct full documents if configured
	if m.config.Results.ReturnFullContent {
		fusedResults = m.reconstructFullDocuments(ctx, fusedResults)
	}

	// Optionally deduplicate based on the final content that will be returned
	// (full documents or chunks).
	if m.config.Results.Deduplicate {
		fusedResults = m.deduplicateResults(fusedResults)
		slog.DebugContext(ctx, "[RAG Manager] Deduplicated fused results",
			"rag_name", m.name,
			"num_results", len(fusedResults))
	}

	// TODO: Track and emit query embedding usage
	// For queries during agent execution, usage should be added to agent's session
	// This requires passing session context through the RAG tool

	return fusedResults, nil
}

// Helper to get strategy names for logging
func getStrategyNames(stratMap map[string]strategy.Strategy) []string {
	return slices.Collect(maps.Keys(stratMap))
}

// CheckAndReindexChangedFiles checks for file changes and re-indexes if needed
func (m *Manager) CheckAndReindexChangedFiles(ctx context.Context) error {
	for strategyName, strategyImpl := range m.strategies {
		strategyCfg := m.strategyConfigs[strategyName]
		if err := strategyImpl.CheckAndReindexChangedFiles(ctx, strategyCfg.Docs, strategyCfg.Chunking); err != nil {
			return fmt.Errorf("strategy %s failed: %w", strategyName, err)
		}
	}
	return nil
}

// StartFileWatcher starts monitoring files and directories for changes
func (m *Manager) StartFileWatcher(ctx context.Context) error {
	for strategyName, strategyImpl := range m.strategies {
		strategyCfg := m.strategyConfigs[strategyName]
		if err := strategyImpl.StartFileWatcher(ctx, strategyCfg.Docs, strategyCfg.Chunking); err != nil {
			return fmt.Errorf("strategy %s failed: %w", strategyName, err)
		}
	}
	return nil
}

// Events returns the event channel shared by all strategies and RAG operations for this manager.
func (m *Manager) Events() <-chan types.Event {
	return m.events
}

// Close closes the manager and releases resources
func (m *Manager) Close() error {
	slog.Debug("[RAG Manager] Closing manager", "rag_name", m.name)

	var firstErr error

	// Close all strategies (which closes their databases and file watchers)
	for strategyName, strategyImpl := range m.strategies {
		if err := strategyImpl.Close(); err != nil {
			slog.Error("[RAG Manager] Failed to close strategy",
				"rag_name", m.name,
				"strategy", strategyName,
				"error", err)
			if firstErr == nil {
				firstErr = fmt.Errorf("failed to close strategy %s: %w", strategyName, err)
			}
		}
	}

	slog.Debug("[RAG Manager] Manager closed", "rag_name", m.name)
	return firstErr
}

// Name returns the RAG source name
func (m *Manager) Name() string {
	return m.name
}

// Description returns the RAG source description
func (m *Manager) Description() string {
	return m.config.Tool.Description
}

// ToolName returns the custom tool name for this RAG source
func (m *Manager) ToolName() string {
	return m.config.Tool.Name
}

// ToolInstruction returns the tool instruction for this RAG source
func (m *Manager) ToolInstruction() string {
	return m.config.Tool.Instruction
}

// deduplicateResults removes duplicate entries from the result set.
// Entries are considered duplicates if they have identical content in Document.Content.
// The first occurrence (highest-ranked result) is kept.
func (m *Manager) deduplicateResults(results []database.SearchResult) []database.SearchResult {
	if len(results) == 0 {
		return results
	}

	seen := make(map[string]struct{}, len(results))
	deduped := make([]database.SearchResult, 0, len(results))

	for _, r := range results {
		key := r.Document.Content
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		deduped = append(deduped, r)
	}

	return deduped
}

// reconstructFullDocuments replaces chunk content with full document content by reading files directly
func (m *Manager) reconstructFullDocuments(_ context.Context, results []database.SearchResult) []database.SearchResult {
	if len(results) == 0 {
		return results
	}

	slog.Debug("[RAG Manager] Reading full documents from disk",
		"rag_name", m.name,
		"num_results", len(results))

	fullContentCache := make(map[string]string)

	for i := range results {
		sourcePath := results[i].Document.SourcePath

		fullContent, ok := fullContentCache[sourcePath]
		if !ok {
			content, err := m.readFile(sourcePath)
			if err != nil {
				slog.Warn("[RAG Manager] Failed to read full document, keeping chunk",
					"rag_name", m.name,
					"source_path", sourcePath,
					"error", err)
				continue
			}

			fullContent = content
			fullContentCache[sourcePath] = fullContent

			slog.Debug("[RAG Manager] Read full document",
				"rag_name", m.name,
				"source_path", sourcePath,
				"length", len(fullContent))
		}

		results[i].Document.Content = fullContent
		results[i].Document.ChunkIndex = 0 // Reset to 0 since it's now the full document
	}

	return results
}

// readFile reads the content of a file
func (m *Manager) readFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("failed to read file %s: %w", path, err)
	}
	return string(data), nil
}

// GetAbsolutePaths converts doc paths to absolute paths relative to basePath.
// If basePath is empty (e.g. for OCI/URL sources), relative paths are resolved
// against the current working directory.
func GetAbsolutePaths(basePath string, docPaths []string) []string {
	var absPaths []string
	for _, p := range docPaths {
		if filepath.IsAbs(p) {
			absPaths = append(absPaths, p)
			continue
		}
		if basePath == "" {
			slog.Debug("Resolving relative path with empty basePath, using working directory", "path", p)
			abs, err := filepath.Abs(p)
			if err != nil {
				slog.Warn("Failed to resolve absolute path, using as-is", "path", p, "error", err)
				absPaths = append(absPaths, p)
			} else {
				absPaths = append(absPaths, abs)
			}
		} else {
			absPaths = append(absPaths, filepath.Join(basePath, p))
		}
	}
	return absPaths
}
