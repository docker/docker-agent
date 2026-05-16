package strategy

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/js"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/rag/chunk"
	"github.com/docker/docker-agent/pkg/rag/types"
	"github.com/docker/docker-agent/pkg/tools"
)

// NewSemanticEmbeddingsFromConfig creates a semantic-embeddings strategy from configuration.
//
// This strategy uses an LLM to generate semantic summaries of each chunk before embedding.
// The summaries capture the meaning/purpose of the code, making retrieval more semantic
// than direct chunk embedding.
//
// Configuration (in RAGStrategyConfig.Params):
//
//   - embedding_model (string, required): embedding model name (same as chunked-embeddings)
//   - chat_model (string, required): chat model used to generate semantic
//     representations for each chunk (e.g., "anthropic/claude-sonnet-4-5")
//   - vector_dimensions (int, required): embedding vector dimensions
//   - semantic_prompt (string, optional): prompt template for the semantic LLM
//   - ast_context (bool, optional): when true, include TreeSitter-derived AST
//     metadata in the semantic prompt (requires chunking.code_aware for best results)
//   - similarity_metric (string, optional): "cosine_similarity" (default) or "euclidean"
//   - threshold (float, optional): minimum similarity score (default: 0.5)
//   - embedding_batch_size (int, optional): batch size for embedding calls (default: 50)
//   - max_embedding_concurrency (int, optional): parallel embedding/LLM calls (default: 3)
//   - max_indexing_concurrency (int, optional): parallel file indexing (default: 3)
//
// # Template Placeholders
//
// Templates use JavaScript template literal syntax (${variable}). The following
// placeholders are available:
//
//   - ${path}         - full source file path
//   - ${basename}     - base name of the source file
//   - ${chunk_index}  - numeric index of the chunk
//   - ${content}      - raw chunk content
//   - ${ast_context}  - formatted AST metadata (empty when unavailable)
//
// If semantic_prompt is omitted, a sensible default is used.
func NewSemanticEmbeddingsFromConfig(ctx context.Context, cfg latest.RAGStrategyConfig, buildCtx BuildContext, events chan<- types.Event) (*Config, error) {
	const strategyName = "semantic-embeddings"

	// Extract required embedding model parameter
	embeddingModelName := GetParam(cfg.Params, "embedding_model", "")
	if embeddingModelName == "" {
		return nil, fmt.Errorf("'embedding_model' parameter required for %s strategy", strategyName)
	}

	// Extract required chat model parameter
	chatModelName := GetParam(cfg.Params, "chat_model", "")
	if strings.TrimSpace(chatModelName) == "" {
		return nil, fmt.Errorf("'chat_model' parameter required for %s strategy", strategyName)
	}

	// vector_dimensions is required
	vectorDimensionsPtr := GetParamPtr[int](cfg.Params, "vector_dimensions")
	if vectorDimensionsPtr == nil {
		return nil, fmt.Errorf("'vector_dimensions' parameter required for %s strategy", strategyName)
	}
	vectorDimensions := *vectorDimensionsPtr

	// Create embedding provider
	embeddingCfg, err := CreateEmbeddingProvider(ctx, embeddingModelName, buildCtx)
	if err != nil {
		return nil, err
	}

	// Create chat model provider
	chatModelCfg, err := ResolveModelConfig(chatModelName, buildCtx.Models)
	if err != nil {
		return nil, fmt.Errorf("invalid chat_model %q: %w", chatModelName, err)
	}

	chatProvider, err := buildCtx.NewProvider(ctx, &chatModelCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create chat model provider: %w", err)
	}

	chatModelID := chatProvider.ID()
	if chatModelID.IsZero() && chatModelCfg.Provider != "" && chatModelCfg.Model != "" {
		chatModelID = modelsdev.NewID(chatModelCfg.Provider, chatModelCfg.Model)
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

	// Read optional semantic_prompt and ast_context parameters
	semanticPrompt := GetParam(cfg.Params, "semantic_prompt", defaultSemanticPrompt())
	useASTContext := GetParam(cfg.Params, "ast_context", false)
	if useASTContext && !cfg.Chunking.CodeAware {
		slog.WarnContext(ctx, "semantic-embeddings ast_context is enabled but chunking.code_aware is false; AST metadata may be unavailable",
			"rag", buildCtx.RAGName)
	}

	// Merge document paths
	docs := MergeDocPaths(buildCtx.SharedDocs, cfg.Docs, buildCtx.ParentDir)

	// Resolve database path
	dbPath, err := ResolveDatabasePath(cfg.Database, buildCtx.ParentDir,
		fmt.Sprintf("rag_%s_semantic_embeddings.db", buildCtx.RAGName))
	if err != nil {
		return nil, fmt.Errorf("invalid database config: %w", err)
	}

	// Create semantic vector database (includes embedding_input column for debugging)
	db, err := newSemanticVectorDB(dbPath, vectorDimensions, strategyName)
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

	// Create usage tracker for chat LLM calls
	usageTracker := func(ctx context.Context, usage *chat.Usage) {
		if usage == nil {
			return
		}

		totalTokens := usage.InputTokens +
			usage.OutputTokens +
			usage.ReasoningTokens +
			usage.CachedInputTokens +
			usage.CacheWriteTokens
		if totalTokens == 0 {
			return
		}

		cost := calculateSemanticUsageCost(embeddingCfg.ModelsStore, chatModelID, usage)
		store.RecordUsage(totalTokens, cost)
	}

	// Configure the embedding input builder to use the chat LLM
	store.SetEmbeddingInputBuilder(newLLMSemanticEmbeddingBuilder(
		chatProvider, js.NewJsExpander(buildCtx.Env), semanticPrompt, usageTracker, useASTContext))

	return &Config{
		Name:      strategyName,
		Strategy:  store,
		Docs:      docs,
		Limit:     limit,
		Threshold: threshold,
		Chunking:  chunkingCfg,
	}, nil
}

// defaultSemanticPrompt returns the default prompt template used to generate
// semantic summaries for code chunks.
func defaultSemanticPrompt() string {
	return `You are summarizing source code for semantic search and RAG.

File path: ${path}
Chunk index: ${chunk_index}
${ast_context}

` + "```" + `
${content}
` + "```" + `

In 2-4 sentences, explain what this code does. Be specific about identifiers:
- Name the exact functions, types, and methods involved
- Mention key dependencies or libraries used (e.g., "uses yaml.Strict()", "calls http.Get()")
- Describe inputs, outputs, and notable behavior

Don't paraphrase - say "Parse() unmarshals YAML into Config" not "parses data into a struct".
Include error handling patterns and edge cases if present.`
}

// llmSemanticEmbeddingBuilder uses a chat model to generate a semantic summary
// for each chunk before it is embedded.
type llmSemanticEmbeddingBuilder struct {
	provider     provider.Provider
	expander     *js.Expander
	prompt       string
	usageTracker func(ctx context.Context, usage *chat.Usage)
	astContext   bool
}

// newLLMSemanticEmbeddingBuilder creates a new embedding input builder that
// calls the given provider with the configured prompt template.
func newLLMSemanticEmbeddingBuilder(
	p provider.Provider,
	expander *js.Expander,
	prompt string,
	usageTracker func(ctx context.Context, usage *chat.Usage),
	astContext bool,
) EmbeddingInputBuilder {
	return &llmSemanticEmbeddingBuilder{
		provider:     p,
		expander:     expander,
		prompt:       prompt,
		usageTracker: usageTracker,
		astContext:   astContext,
	}
}

func (b *llmSemanticEmbeddingBuilder) BuildEmbeddingInput(ctx context.Context, sourcePath string, ch chunk.Chunk) (string, error) {
	// Fill in the prompt template with code-specific context
	astContext := ""
	if b.astContext {
		astContext = formatASTContext(ch.Metadata)
	}

	t := b.expander.Expand(ctx, b.prompt, map[string]string{
		"path":        sourcePath,
		"basename":    filepath.Base(sourcePath),
		"chunk_index": strconv.Itoa(ch.Index),
		"content":     ch.Content,
		"ast_context": astContext,
	})

	messages := []chat.Message{
		{
			Role:    chat.MessageRoleUser,
			Content: t,
		},
	}

	stream, err := b.provider.CreateChatCompletionStream(ctx, messages, []tools.Tool{})
	if err != nil {
		return "", fmt.Errorf("failed to start semantic generation: %w", err)
	}
	defer stream.Close()

	var (
		sb    strings.Builder
		usage *chat.Usage
	)

	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("error receiving from semantic generation stream: %w", err)
		}

		if resp.Usage != nil {
			usage = resp.Usage
		}

		for _, choice := range resp.Choices {
			if choice.Delta.Content != "" {
				sb.WriteString(choice.Delta.Content)
			}
		}
	}

	summary := strings.TrimSpace(sb.String())

	if usage != nil && b.usageTracker != nil {
		b.usageTracker(ctx, usage)
	}

	// Maximum length for embedding input to stay within model limits.
	const maxEmbeddingInputLength = 2000

	if summary == "" {
		// If the semantic model returns no content, fall back to truncated chunk
		slog.WarnContext(ctx, "Semantic model returned empty summary; falling back to truncated chunk content",
			"path", sourcePath,
			"chunk_index", ch.Index,
			"original_length", len(ch.Content))

		fallback := ch.Content
		if len(fallback) > maxEmbeddingInputLength {
			fallback = fallback[:maxEmbeddingInputLength] + "..."
			slog.DebugContext(ctx, "Truncated fallback content to fit embedding model limits",
				"original_length", len(ch.Content),
				"truncated_length", len(fallback))
		}
		return fallback, nil
	}

	// Build the final embedding input by prepending structured metadata to the summary.
	// This ensures that key identifiers (function names, packages, signatures) are present
	// in the embedding, improving retrieval for queries that use code identifiers.
	embeddingInput := buildEmbeddingInputWithMetadata(sourcePath, ch.Metadata, summary)

	// Truncate if embedding input is unexpectedly long
	if len(embeddingInput) > maxEmbeddingInputLength {
		slog.WarnContext(ctx, "Semantic embedding input exceeds model limits; truncating",
			"path", sourcePath,
			"chunk_index", ch.Index,
			"original_length", len(embeddingInput),
			"truncated_length", maxEmbeddingInputLength)
		embeddingInput = embeddingInput[:maxEmbeddingInputLength] + "..."
	}

	slog.DebugContext(ctx, "Generated semantic embedding input for chunk",
		"path", sourcePath,
		"chunk_index", ch.Index,
		"embedding_input", embeddingInput)

	return embeddingInput, nil
}

// buildEmbeddingInputWithMetadata constructs the final text to be embedded by
// prepending structured metadata to the LLM-generated summary. This hybrid approach
// ensures that:
//  1. Key identifiers (function names, packages, signatures) are present verbatim
//     for better keyword matching
//  2. The semantic summary provides conceptual understanding for similarity search
//
// Example output:
//
//	File: pkg/config/parser.go
//	Function: Parse (function)
//	Package: config
//	Signature: func Parse(data []byte) (*Config, error)
//
//	This function parses YAML-formatted byte data into a Config struct...
func buildEmbeddingInputWithMetadata(sourcePath string, metadata map[string]string, summary string) string {
	var sb strings.Builder

	// Always include the file path for context
	sb.WriteString("File: ")
	sb.WriteString(sourcePath)
	sb.WriteString("\n")

	// Add symbol name and kind if available
	if name := metadata["symbol_name"]; name != "" {
		sb.WriteString("Function: ")
		sb.WriteString(name)
		if kind := metadata["symbol_kind"]; kind != "" {
			sb.WriteString(" (")
			sb.WriteString(kind)
			sb.WriteString(")")
		}
		sb.WriteString("\n")
	}

	// Add receiver for methods
	if receiver := metadata["receiver"]; receiver != "" {
		sb.WriteString("Receiver: ")
		sb.WriteString(receiver)
		sb.WriteString("\n")
	}

	// Add package name
	if pkg := metadata["package"]; pkg != "" {
		sb.WriteString("Package: ")
		sb.WriteString(pkg)
		sb.WriteString("\n")
	}

	// Add signature - this is crucial for matching function signatures
	if sig := metadata["signature"]; sig != "" {
		sb.WriteString("Signature: ")
		sb.WriteString(sig)
		sb.WriteString("\n")
	}

	// Add any additional symbols in the chunk
	if additional := metadata["additional_symbols"]; additional != "" {
		sb.WriteString("Also contains: ")
		sb.WriteString(additional)
		sb.WriteString("\n")
	}

	// Add blank line before summary
	sb.WriteString("\n")
	sb.WriteString(summary)

	return sb.String()
}

// formatASTContext formats chunk metadata as human-readable AST context.
func formatASTContext(metadata map[string]string) string {
	if len(metadata) == 0 {
		return ""
	}

	type keyLabel struct {
		key   string
		label string
	}

	keyOrder := []keyLabel{
		{key: "symbol_name", label: "Symbol"},
		{key: "symbol_kind", label: "Kind"},
		{key: "receiver", label: "Receiver"},
		{key: "signature", label: "Signature"},
		{key: "doc", label: "Doc"},
		{key: "package", label: "Package"},
		{key: "start_line", label: "Start line"},
		{key: "end_line", label: "End line"},
		{key: "additional_symbols", label: "Additional symbols"},
		{key: "symbol_count", label: "Symbol count"},
	}

	const maxLines = 8
	lines := make([]string, 0, maxLines)
	used := make(map[string]bool, len(keyOrder))

	for _, entry := range keyOrder {
		value := strings.TrimSpace(metadata[entry.key])
		if value == "" {
			continue
		}
		lines = append(lines, fmt.Sprintf("- %s: %s", entry.label, value))
		used[entry.key] = true
		if len(lines) >= maxLines {
			break
		}
	}

	if len(lines) < maxLines {
		extraKeys := make([]string, 0, len(metadata))
		for key := range metadata {
			if used[key] {
				continue
			}
			extraKeys = append(extraKeys, key)
		}

		slices.Sort(extraKeys)
		for _, key := range extraKeys {
			value := strings.TrimSpace(metadata[key])
			if value == "" {
				continue
			}
			lines = append(lines, fmt.Sprintf("- %s: %s", humanizeMetadataKey(key), value))
			if len(lines) >= maxLines {
				break
			}
		}
	}

	if len(lines) == 0 {
		return ""
	}

	return "AST context:\n" + strings.Join(lines, "\n")
}

// humanizeMetadataKey converts snake_case keys to Title Case.
func humanizeMetadataKey(key string) string {
	parts := strings.Split(key, "_")
	for i, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lower := strings.ToLower(part)
		if len(lower) == 1 {
			parts[i] = strings.ToUpper(lower)
			continue
		}
		parts[i] = strings.ToUpper(lower[:1]) + lower[1:]
	}
	return strings.Join(parts, " ")
}

// calculateSemanticUsageCost calculates cost for semantic LLM usage.
func calculateSemanticUsageCost(modelsStore modelStore, id modelsdev.ID, usage *chat.Usage) float64 {
	if usage == nil || modelsStore == nil || !id.IsValid() || id.Provider == "dmr" {
		return 0
	}

	model, err := modelsStore.GetModel(context.Background(), id)
	if err != nil {
		slog.Debug("Failed to get semantic model pricing from models.dev, cost will be 0",
			"model_id", id.String(),
			"error", err)
		return 0
	}

	if model.Cost == nil {
		return 0
	}

	inputCost := float64(usage.InputTokens) * model.Cost.Input
	outputCost := float64(usage.OutputTokens+usage.ReasoningTokens) * model.Cost.Output
	cacheReadCost := float64(usage.CachedInputTokens) * model.Cost.CacheRead
	cacheWriteCost := float64(usage.CacheWriteTokens) * model.Cost.CacheWrite

	return (inputCost + outputCost + cacheReadCost + cacheWriteCost) / 1e6
}
