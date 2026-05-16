package strategy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/rag/embed"
)

// EmbeddingConfig holds configuration for creating an embedding provider.
type EmbeddingConfig struct {
	Provider    provider.Provider
	ModelID     modelsdev.ID // Provider/model identity, used for pricing lookup.
	ModelsStore *modelsdev.Store
}

// CreateEmbeddingProvider creates an embedding model provider from configuration.
// Supports "auto" for auto-detection, inline "provider/model" format, or named model references.
func CreateEmbeddingProvider(ctx context.Context, modelName string, buildCtx BuildContext) (*EmbeddingConfig, error) {
	var embedModel provider.Provider
	var modelCfg latest.ModelConfig
	var err error

	if modelName == "auto" {
		// Auto-detect embedding model (try OpenAI first, fall back to DMR)
		embedModel, err = createAutoEmbeddingModel(ctx, buildCtx)
		if err != nil {
			return nil, fmt.Errorf("failed to create auto embedding model: %w", err)
		}
	} else {
		// Look up or parse model config
		modelCfg, err = ResolveModelConfig(modelName, buildCtx.Models)
		if err != nil {
			return nil, fmt.Errorf("model '%s' not found: %w", modelName, err)
		}

		embedModel, err = buildCtx.NewProvider(ctx, &modelCfg)
		if err != nil {
			return nil, fmt.Errorf("failed to create embedding model: %w", err)
		}
	}

	// Determine model ID for pricing lookup
	var modelID modelsdev.ID
	if modelName == "auto" {
		modelID = embedModel.ID()
	} else {
		modelID = modelsdev.NewID(modelCfg.Provider, modelCfg.Model)
	}

	var modelsStore *modelsdev.Store
	if buildCtx.RuntimeConfig != nil {
		var err error
		modelsStore, err = buildCtx.RuntimeConfig.ModelsDevStore()
		if err != nil {
			slog.DebugContext(ctx, "Failed to create models.dev store for RAG pricing; cost tracking disabled",
				"error", err)
		}
	}

	return &EmbeddingConfig{
		Provider:    embedModel,
		ModelID:     modelID,
		ModelsStore: modelsStore,
	}, nil
}

// createAutoEmbeddingModel creates an auto-detected embedding model.
func createAutoEmbeddingModel(ctx context.Context, buildCtx BuildContext) (provider.Provider, error) {
	var lastErr error

	for _, autoModelCfg := range config.AutoEmbeddingModelConfigs() {
		modelCfg := latest.ModelConfig{
			Provider: autoModelCfg.Provider,
			Model:    autoModelCfg.Model,
		}

		model, err := buildCtx.NewProvider(ctx, &modelCfg)
		if err != nil {
			lastErr = err
			continue
		}

		return model, nil
	}

	if lastErr == nil {
		return nil, errors.New("failed to create auto embedding model: no candidates configured")
	}

	return nil, fmt.Errorf("failed to create auto embedding model: %w", lastErr)
}

// CreateEmbedder creates an embedder with the specified configuration.
func CreateEmbedder(embedModel provider.Provider, batchSize, maxConcurrency int) *embed.Embedder {
	return embed.New(embedModel, embed.WithBatchSize(batchSize), embed.WithMaxConcurrency(maxConcurrency))
}
