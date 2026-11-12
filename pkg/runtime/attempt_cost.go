package runtime

import (
	"context"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/runtime/pricing"
)

// attemptsCost prices a response a server fallback spread across models: each
// billed attempt at its own model's rates, never the top-level usage, which
// only repeats the last attempt. The dispatched attempt keeps its config,
// display alias and cost override; other models are priced from the
// catalogue. Nil when any billed attempt cannot be priced.
func attemptsCost(ctx context.Context, attempts []chat.UsageAttempt, dispatched base.Config, store ModelStore) *float64 {
	var total float64
	for _, attempt := range attempts {
		if !attempt.Billed {
			continue
		}
		cfg := dispatched
		if !attempt.Dispatched {
			cfg.ModelConfig.Model = attempt.Model
			cfg.ModelConfig.DisplayModel = ""
			cfg.ModelConfig.Cost = nil
		}
		id := cfg.ID()
		m, err := store.GetModel(ctx, id)
		if err != nil {
			m = nil
		}
		usage := attempt.Usage()
		cost := pricing.ComputeMessageCost(usage, pricing.ApplyModelCost(m, id, usage, cfg))
		if cost == nil {
			return nil
		}
		total += *cost
	}
	return &total
}
