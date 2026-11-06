package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
)

// catalogStore prices each model ID from its own table; others are unknown.
type catalogStore struct {
	ModelStore

	costs map[string]modelsdev.Cost
}

func (s catalogStore) GetModel(_ context.Context, id modelsdev.ID) (*modelsdev.Model, error) {
	c, ok := s.costs[id.String()]
	if !ok {
		return nil, errors.New("not in catalogue")
	}
	return &modelsdev.Model{Cost: &c}, nil
}

func TestAttemptsCost(t *testing.T) {
	t.Parallel()

	store := catalogStore{costs: map[string]modelsdev.Cost{
		"anthropic/claude-opus-4-8":   {Input: 10, Output: 50},
		"anthropic/claude-sonnet-4-6": {Input: 3, Output: 15},
	}}
	dispatched := base.Config{ModelConfig: latest.ModelConfig{Provider: "anthropic", Model: "claude-opus-4-8"}}
	declined := chat.UsageAttempt{Model: "claude-opus-4-8", Billed: true, Dispatched: true, InputTokens: 1_000_000, OutputTokens: 100_000}
	served := chat.UsageAttempt{Model: "claude-sonnet-4-6", Billed: true, InputTokens: 1_000_000, OutputTokens: 200_000}

	for name, tc := range map[string]struct {
		attempts   []chat.UsageAttempt
		dispatched base.Config
		want       *float64
	}{
		// $10 + $5 at Opus rates, $3 + $3 at Sonnet rates.
		"each attempt at its own rates": {attempts: []chat.UsageAttempt{declined, served}, dispatched: dispatched, want: new(21.0)},
		"an unbilled declined attempt costs nothing": {
			attempts:   []chat.UsageAttempt{{Model: "claude-opus-4-8", InputTokens: 1_000_000}, served},
			dispatched: dispatched, want: new(6.0),
		},
		"an uncatalogued attempt leaves the turn unpriced": {
			attempts:   []chat.UsageAttempt{declined, {Model: "claude-unknown", Billed: true, InputTokens: 1}},
			dispatched: dispatched,
		},
		// The API may report a resolved, dated ID for the requested model.
		"the dispatched attempt keeps its config under a resolved ID": {
			attempts: []chat.UsageAttempt{{Model: "claude-opus-4-8-20260901", Billed: true, Dispatched: true, InputTokens: 1_000_000, OutputTokens: 100_000}, served},
			dispatched: base.Config{ModelConfig: latest.ModelConfig{
				Provider: "anthropic", Model: "claude-opus-4-8", Cost: &latest.CostConfig{Input: 1, Output: 1},
			}},
			want: new(7.1),
		},
		"the dispatched model keeps its alias and cost override": {
			attempts: []chat.UsageAttempt{declined, served},
			dispatched: base.Config{ModelConfig: latest.ModelConfig{
				Provider: "anthropic", Model: "claude-opus-4-8", DisplayModel: "opus",
				Cost: &latest.CostConfig{Input: 1, Output: 1},
			}},
			// $1 + $0.1 from the override, $6 at Sonnet rates.
			want: new(7.1),
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := attemptsCost(t.Context(), tc.attempts, tc.dispatched, store)
			if tc.want == nil {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.InDelta(t, *tc.want, *got, 1e-9)
		})
	}
}

// Behind a router, the requested model's attempt is priced with the leaf the
// router dispatched to, not the router's own config.
func TestDispatchedConfig(t *testing.T) {
	t.Parallel()

	router := base.Config{ModelConfig: latest.ModelConfig{Provider: "anthropic", Model: "router"}}
	leaf := provider.ServingModel{Config: base.Config{ModelConfig: latest.ModelConfig{Provider: "anthropic", Model: "claude-sonnet-5-5"}}}
	assert.Equal(t, router, dispatchedConfig(streamResult{}, router))
	assert.Equal(t, leaf.Config, dispatchedConfig(streamResult{Dispatched: &leaf}, router))
}
