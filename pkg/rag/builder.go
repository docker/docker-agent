package rag

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/rag/rerank"
	"github.com/docker/docker-agent/pkg/rag/strategy"
	"github.com/docker/docker-agent/pkg/rag/types"
)

// ManagersBuildConfig contains dependencies needed to build RAG managers from config.
type ManagersBuildConfig struct {
	ParentDir     string
	ModelsGateway string
	Env           environment.Provider
	Models        map[string]latest.ModelConfig    // Model configurations from config
	Providers     map[string]latest.ProviderConfig // Custom provider configurations from config
	RuntimeConfig *config.RuntimeConfig
}

// NewProvider creates a model provider using the build config's environment,
// gateway, and custom provider settings.
func (c ManagersBuildConfig) NewProvider(ctx context.Context, cfg *latest.ModelConfig) (provider.Provider, error) {
	return provider.New(ctx, cfg, c.Env,
		options.WithGateway(c.ModelsGateway),
		options.WithProviders(c.Providers))
}

// NewManager constructs a single RAG manager from a RAGConfig.
func NewManager(
	ctx context.Context,
	ragName string,
	ragCfg *latest.RAGConfig,
	buildCfg ManagersBuildConfig,
) (*Manager, error) {
	if ragCfg == nil {
		return nil, fmt.Errorf("nil RAG config for %q", ragName)
	}

	// Validate that we have at least one strategy
	if len(ragCfg.Strategies) == 0 {
		return nil, fmt.Errorf("no strategies configured for RAG %q", ragName)
	}

	// Build context for strategy builders
	strategyBuildCtx := strategy.BuildContext{
		RAGName:       ragName,
		ParentDir:     buildCfg.ParentDir,
		SharedDocs:    GetAbsolutePaths(buildCfg.ParentDir, ragCfg.Docs),
		Models:        buildCfg.Models,
		Providers:     buildCfg.Providers,
		Env:           buildCfg.Env,
		ModelsGateway: buildCfg.ModelsGateway,
		RespectVCS:    ragCfg.GetRespectVCS(),
		RuntimeConfig: buildCfg.RuntimeConfig,
	}

	strategyConfigs, strategyEvents, err := buildStrategyConfigs(ctx, *ragCfg, strategyBuildCtx, ragName)
	if err != nil {
		return nil, fmt.Errorf("failed to build strategy configs for RAG %q: %w", ragName, err)
	}

	managerCfg, err := buildManagerConfig(ctx, *ragCfg, buildCfg, strategyConfigs)
	if err != nil {
		return nil, fmt.Errorf("failed to build manager config for RAG %q: %w", ragName, err)
	}

	manager, err := New(ctx, ragName, managerCfg, strategyEvents)
	if err != nil {
		return nil, fmt.Errorf("failed to create RAG manager %q: %w", ragName, err)
	}

	strategyNames := make([]string, len(strategyConfigs))
	for i, sc := range strategyConfigs {
		strategyNames[i] = sc.Name
	}
	slog.DebugContext(ctx, "Created RAG manager",
		"name", ragName,
		"strategies", strategyNames,
		"docs", len(managerCfg.Docs))

	return manager, nil
}

// buildManagerConfig constructs a rag.Manager Config from the configuration and strategies.
func buildManagerConfig(
	ctx context.Context,
	ragCfg latest.RAGConfig,
	buildCfg ManagersBuildConfig,
	strategyConfigs []strategy.Config,
) (Config, error) {
	results := ResultsConfig{
		Limit:             ragCfg.Results.Limit,
		Deduplicate:       ragCfg.Results.Deduplicate,
		IncludeScore:      ragCfg.Results.IncludeScore,
		ReturnFullContent: ragCfg.Results.ReturnFullContent,
	}

	// Build reranking config if configured
	if ragCfg.Results.Reranking != nil {
		slog.DebugContext(ctx, "Building reranking configuration",
			"model", ragCfg.Results.Reranking.Model,
			"top_k", ragCfg.Results.Reranking.TopK,
			"threshold", ragCfg.Results.Reranking.Threshold)

		rerankingCfg, err := buildRerankingConfig(ctx, ragCfg.Results.Reranking, buildCfg, results.Limit)
		if err != nil {
			slog.ErrorContext(ctx, "Failed to build reranking config",
				"model", ragCfg.Results.Reranking.Model,
				"error", err)
			return Config{}, fmt.Errorf("failed to build reranking config: %w", err)
		}
		results.RerankingConfig = rerankingCfg
		slog.DebugContext(ctx, "Reranking configuration built successfully",
			"model", ragCfg.Results.Reranking.Model)
	}

	fusionCfg := buildManagerFusionConfig(ragCfg, strategyConfigs)

	return Config{
		Tool: ToolConfig{
			Name:        ragCfg.Tool.Name,
			Description: ragCfg.Tool.Description,
			Instruction: ragCfg.Tool.Instruction,
		},
		Docs:            GetAbsolutePaths(buildCfg.ParentDir, ragCfg.Docs),
		Results:         results,
		FusionConfig:    fusionCfg,
		StrategyConfigs: strategyConfigs,
	}, nil
}

// buildRerankingConfig constructs a RerankingConfig from the configuration.
func buildRerankingConfig(
	ctx context.Context,
	rerankCfg *latest.RAGRerankingConfig,
	buildCfg ManagersBuildConfig,
	globalLimit int,
) (*RerankingConfig, error) {
	if rerankCfg == nil {
		return nil, nil
	}

	if rerankCfg.Model == "" {
		slog.ErrorContext(ctx, "Reranking model name is empty")
		return nil, errors.New("reranking model is required")
	}

	slog.DebugContext(ctx, "Resolving reranking model",
		"model_ref", rerankCfg.Model)

	// Resolve model config - check if it's a reference to a defined model or inline
	modelCfgVal, err := strategy.ResolveModelConfig(rerankCfg.Model, buildCfg.Models)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to resolve reranking model",
			"model_ref", rerankCfg.Model,
			"error", err)
		return nil, fmt.Errorf("failed to resolve reranking model %q: %w", rerankCfg.Model, err)
	}
	modelCfg := &modelCfgVal

	slog.DebugContext(ctx, "Resolved reranking model config",
		"provider", modelCfg.Provider,
		"model", modelCfg.Model)

	// Create provider for reranking model
	rerankProvider, err := buildCfg.NewProvider(ctx, modelCfg)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to create reranking provider",
			"provider", modelCfg.Provider,
			"model", modelCfg.Model,
			"error", err)
		return nil, fmt.Errorf("failed to create reranking provider: %w", err)
	}

	slog.DebugContext(ctx, "Created reranking provider",
		"provider_id", rerankProvider.ID())

	// Determine effective TopK:
	// - If user provided a positive top_k, respect it.
	// - Otherwise, default to the global results limit when set.
	//   This avoids reranking unbounded result sets while still
	//   using a sensible, user-controlled cap.
	effectiveTopK := rerankCfg.TopK
	if effectiveTopK <= 0 && globalLimit > 0 {
		effectiveTopK = globalLimit
	}

	// Create reranker
	reranker, err := rerank.NewLLMReranker(rerank.Config{
		Model:     rerankProvider,
		TopK:      effectiveTopK,
		Threshold: rerankCfg.Threshold,
		Criteria:  rerankCfg.Criteria,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create reranker: %w", err)
	}

	slog.InfoContext(ctx, "Built reranking configuration successfully",
		"model", rerankCfg.Model,
		"provider_id", rerankProvider.ID(),
		"top_k", effectiveTopK,
		"threshold", rerankCfg.Threshold,
		"has_criteria", rerankCfg.Criteria != "")

	return &RerankingConfig{
		Reranker:  reranker,
		TopK:      effectiveTopK,
		Threshold: rerankCfg.Threshold,
	}, nil
}

// buildStrategyConfigs builds the strategy configs for the RAG.
// Returns a slice of strategy configs and a channel for receiving strategy events.
func buildStrategyConfigs(
	ctx context.Context,
	ragCfg latest.RAGConfig,
	strategyBuildCtx strategy.BuildContext,
	ragName string,
) ([]strategy.Config, chan types.Event, error) {
	// Create event channel for strategies to emit events.
	// This channel is shared with the manager, which exposes it directly to callers.
	// Use generous buffer to prevent blocking during heavy indexing.
	strategyEvents := make(chan types.Event, 500)

	// Build all strategies for this RAG source
	var strategyConfigs []strategy.Config
	for _, strategyCfg := range ragCfg.Strategies {
		builtStrategy, err := strategy.BuildStrategy(ctx, strategyCfg, strategyBuildCtx, strategyEvents)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to build strategy %q for RAG %q: %w",
				strategyCfg.Type, ragName, err)
		}

		// Copy the built strategy config so callers can't mutate internal state.
		strategyConfigs = append(strategyConfigs, *builtStrategy)
	}

	return strategyConfigs, strategyEvents, nil
}

// buildManagerFusionConfig constructs a FusionConfig from the configuration.
// Returns nil if only one strategy is configured (no fusion needed).
func buildManagerFusionConfig(
	ragCfg latest.RAGConfig,
	strategyConfigs []strategy.Config,
) *FusionConfig {
	// Only use fusion if multiple strategies
	if len(strategyConfigs) <= 1 {
		return nil
	}

	fusionStrategy := "rrf" // Default to Reciprocal Rank Fusion
	fusionK := 60
	var fusionWeights map[string]float64

	if ragCfg.Results.Fusion != nil {
		if ragCfg.Results.Fusion.Strategy != "" {
			fusionStrategy = ragCfg.Results.Fusion.Strategy
		}
		if ragCfg.Results.Fusion.K > 0 {
			fusionK = ragCfg.Results.Fusion.K
		}
		fusionWeights = ragCfg.Results.Fusion.Weights
	}

	return &FusionConfig{
		Strategy: fusionStrategy,
		K:        fusionK,
		Weights:  fusionWeights,
	}
}
