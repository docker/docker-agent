package evaluation

import (
	"context"
	"fmt"
	"maps"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	evaluatorprovider "github.com/docker/docker-agent/pkg/evaluator/provider"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/options"
)

// Judge types select the protocol used for relevance assessments.
const (
	JudgeTypeLLM       = "llm"
	JudgeTypeEvaluator = "evaluator"
)

func createJudge(ctx context.Context, cfg Config, runConfig *config.RuntimeConfig, agentConfig *latest.Config) (*Judge, error) {
	switch cfg.JudgeType {
	case "", JudgeTypeLLM:
		model, err := createJudgeModel(ctx, cfg.JudgeModel, runConfig)
		if err != nil || model == nil {
			return nil, err
		}
		return NewJudge(model, cfg.Concurrency), nil
	case JudgeTypeEvaluator:
		if cfg.JudgeModel == "" {
			return nil, nil
		}
		var def latest.EvaluatorConfig
		var found bool
		if agentConfig != nil {
			def, found = agentConfig.Evaluators[cfg.JudgeModel]
		}
		if !found {
			ref, err := latest.ParseModelRef(cfg.JudgeModel)
			if err != nil {
				return nil, fmt.Errorf("invalid evaluator judge model %q: expected 'provider/model' or a named evaluator", cfg.JudgeModel)
			}
			def = latest.EvaluatorConfig{
				Provider:     ref.Provider,
				Model:        ref.Model,
				Type:         "boolean",
				Instructions: relevanceEvaluatorInstructions,
			}
		}
		if def.Type != "boolean" {
			return nil, fmt.Errorf("evaluator judge %q must use type boolean", cfg.JudgeModel)
		}
		providers := runConfig.Providers
		if agentConfig != nil {
			// Agent definitions win over user-level connection defaults.
			merged := &latest.Config{Providers: maps.Clone(agentConfig.Providers)}
			config.MergeGlobalProviders(merged, providers)
			providers = merged.Providers
		}
		resolved, err := def.Resolve(providers)
		if err != nil {
			return nil, fmt.Errorf("resolving evaluator judge: %w", err)
		}
		opts := []options.Opt{options.WithGateway(runConfig.ModelsGateway), options.WithEncryptedConfig(runConfig.EncryptedConfig)}
		opts = append(opts, runConfig.EvaluatorOptions...)
		client, err := evaluatorprovider.New(ctx, resolved, runConfig.EnvProvider(), opts...)
		if err != nil {
			return nil, fmt.Errorf("creating evaluator judge: %w", err)
		}
		return NewJudgeWithBackend(&evaluatorJudge{client: client}, cfg.Concurrency), nil
	default:
		return nil, fmt.Errorf("invalid judge type %q: expected llm or evaluator", cfg.JudgeType)
	}
}

// createJudgeModel creates a provider.Provider from a model string (format: provider/model).
// Returns nil if judgeModel is empty.
func createJudgeModel(ctx context.Context, judgeModel string, runConfig *config.RuntimeConfig) (provider.Provider, error) {
	if judgeModel == "" {
		return nil, nil
	}

	cfg, err := latest.ParseModelRef(judgeModel)
	if err != nil {
		return nil, fmt.Errorf("invalid judge model format %q: expected 'provider/model'", judgeModel)
	}

	opts := []options.Opt{
		options.WithStructuredOutput(judgeResponseSchema),
	}
	if runConfig.ModelsGateway != "" {
		opts = append(opts, options.WithGateway(runConfig.ModelsGateway))
	}

	judge, err := runConfig.ProviderRegistryOrDefault().New(ctx, &cfg, runConfig.EnvProvider(), opts...)
	if err != nil {
		return nil, fmt.Errorf("creating judge model: %w", err)
	}

	return judge, nil
}
