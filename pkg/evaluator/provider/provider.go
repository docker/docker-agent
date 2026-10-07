// Package provider constructs independently configured evaluator clients.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/evaluator"
)

const defaultTimeout = 10 * time.Second

// New builds a reusable client from a resolved evaluator configuration.
// Credentials are obtained from env on each evaluation, not during construction.
// Without [WithModelsGateway] the client always connects directly.
func New(ctx context.Context, cfg latest.EvaluatorConfig, env environment.Provider, opts ...Option) (evaluator.Evaluator, error) {
	if err := cfg.Validate(); err != nil {
		return nil, errors.New("invalid evaluator configuration")
	}
	if cfg.Provider != latest.EvaluatorProviderTypeSafe && cfg.Provider != latest.EvaluatorProviderOpenAI {
		return nil, errors.New("unsupported evaluator provider")
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("evaluator model is required")
	}
	if env == nil {
		return nil, errors.New("evaluator environment provider is required")
	}
	if cfg.Provider == latest.EvaluatorProviderOpenAI {
		return newOpenAI(ctx, cfg, env, opts)
	}
	return newTypeSafe(ctx, cfg, env, opts)
}

func timeoutOrDefault(cfg latest.EvaluatorConfig) time.Duration {
	if cfg.Timeout.Duration == 0 {
		return defaultTimeout
	}
	return cfg.Timeout.Duration
}

func copyCost(cost *latest.CostConfig) *latest.CostConfig {
	if cost == nil {
		return nil
	}
	return new(*cost)
}

// encodeState validates and marshals an evaluation state before any credential lookup.
func encodeState(state any) (json.RawMessage, error) {
	raw, err := json.Marshal(state)
	if err != nil {
		return nil, errors.New("evaluator state must be JSON-serializable")
	}
	if len(raw) == 0 || (raw[0] != '"' && raw[0] != '{' && raw[0] != '[') {
		return nil, errors.New("evaluator state must be a string, object, or array")
	}
	return raw, nil
}
