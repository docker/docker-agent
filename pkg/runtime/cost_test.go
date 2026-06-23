package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

// TestComputeMessageCost_UncataloguedModelIsUnpriced is the regression test for
// the silent "$0 cost despite token usage" leak: when a model is absent from
// the pricing catalogue (m == nil) or carries no price table (m.Cost == nil),
// the per-message cost is 0 even though real tokens were spent. Before the fix
// the caller could not tell this apart from a genuinely free turn, so a spend
// guardrail built on the cost would never trip. computeMessageCost now reports
// priced=false for exactly these cases.
func TestComputeMessageCost_UncataloguedModelIsUnpriced(t *testing.T) {
	usage := &chat.Usage{InputTokens: 1000, OutputTokens: 500}

	t.Run("model missing from catalogue", func(t *testing.T) {
		cost, priced := computeMessageCost(usage, nil)
		assert.Zero(t, cost)
		assert.False(t, priced, "an uncatalogued model must be reported as unpriced")
	})

	t.Run("model present but no price table", func(t *testing.T) {
		cost, priced := computeMessageCost(usage, &modelsdev.Model{})
		assert.Zero(t, cost)
		assert.False(t, priced, "a model with no Cost table must be reported as unpriced")
	})

	t.Run("nil usage", func(t *testing.T) {
		cost, priced := computeMessageCost(nil, &modelsdev.Model{Cost: &modelsdev.Cost{Input: 1}})
		assert.Zero(t, cost)
		assert.False(t, priced)
	})
}

// TestComputeMessageCost_PricedModel verifies the cost arithmetic is unchanged
// from the original inline formula and that a catalogued model reports priced.
func TestComputeMessageCost_PricedModel(t *testing.T) {
	usage := &chat.Usage{
		InputTokens:       1_000_000,
		OutputTokens:      2_000_000,
		CachedInputTokens: 3_000_000,
		CacheWriteTokens:  4_000_000,
	}
	m := &modelsdev.Model{Cost: &modelsdev.Cost{
		Input:      1.0,
		Output:     2.0,
		CacheRead:  0.5,
		CacheWrite: 3.0,
	}}

	cost, priced := computeMessageCost(usage, m)

	assert.True(t, priced, "a catalogued model with a price table must be reported as priced")
	// (1e6*1 + 2e6*2 + 3e6*0.5 + 4e6*3) / 1e6 = 1 + 4 + 1.5 + 12 = 18.5
	assert.InDelta(t, 18.5, cost, 1e-9)
}

func TestUsageHasTokens(t *testing.T) {
	assert.False(t, usageHasTokens(nil), "nil usage has no tokens")
	assert.False(t, usageHasTokens(&chat.Usage{}), "zero usage has no tokens")
	assert.True(t, usageHasTokens(&chat.Usage{InputTokens: 1}))
	assert.True(t, usageHasTokens(&chat.Usage{OutputTokens: 1}))
	assert.True(t, usageHasTokens(&chat.Usage{CachedInputTokens: 1}))
	assert.True(t, usageHasTokens(&chat.Usage{CacheWriteTokens: 1}))
}
