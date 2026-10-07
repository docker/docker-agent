package provider

import (
	"encoding/json"
	"errors"
	"math"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/evaluator"
)

func reportedUsage(raw json.RawMessage) (*evaluator.Usage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var usage struct {
		InputTokens  *int64 `json:"input_tokens"`
		OutputTokens *int64 `json:"output_tokens"`
	}
	if err := json.Unmarshal(raw, &usage); err != nil {
		return nil, errors.New("evaluator response has invalid token usage")
	}
	if (usage.InputTokens != nil && *usage.InputTokens < 0) || (usage.OutputTokens != nil && *usage.OutputTokens < 0) {
		return nil, errors.New("evaluator response has invalid token usage")
	}
	if usage.InputTokens == nil || usage.OutputTokens == nil {
		return nil, nil
	}
	if *usage.InputTokens > math.MaxInt64-*usage.OutputTokens {
		return nil, errors.New("evaluator response has invalid token usage")
	}
	return &evaluator.Usage{InputTokens: *usage.InputTokens, OutputTokens: *usage.OutputTokens}, nil
}

func (p *typesafe) estimateCost(model string, usage *evaluator.Usage) *float64 {
	price := p.cost
	if price == nil && p.officialPricing && model == "jev-1.13.0" {
		// https://docs.typesafe.ai/models: input $0.042/Mtok, output free.
		price = &latest.CostConfig{Input: 0.042}
	}
	return estimateCost(price, usage)
}

// estimateCost returns nil, meaning unknown, without usage or a price.
func estimateCost(price *latest.CostConfig, usage *evaluator.Usage) *float64 {
	if usage == nil || price == nil {
		return nil
	}
	cost := float64(usage.InputTokens)/1e6*price.Input + float64(usage.OutputTokens)/1e6*price.Output
	if math.IsNaN(cost) || math.IsInf(cost, 0) {
		return nil
	}
	return &cost
}
