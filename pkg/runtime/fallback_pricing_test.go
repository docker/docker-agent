package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

type pricingProvider struct {
	provider.Provider

	cost *latest.CostConfig
}

func (p *pricingProvider) BaseConfig() base.Config {
	return base.Config{ModelConfig: latest.ModelConfig{Cost: p.cost}}
}

func TestFallbackPricing(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name            string
		fallbackID      string
		fallbackCost    *latest.CostConfig
		primarySucceeds bool
		wantCost        float64
		unpriced        bool
	}{
		{name: "catalog tier and cooldown", fallbackID: "test/fallback", wantCost: 3.75},
		{name: "fallback override", fallbackID: "test/fallback", fallbackCost: &latest.CostConfig{Input: 1, Output: 2}, wantCost: 0.5},
		{name: "free fallback override", fallbackID: "test/fallback", fallbackCost: &latest.CostConfig{}, wantCost: 0},
		{name: "unknown fallback", fallbackID: "test/unknown", unpriced: true},
		{name: "fallback without rates", fallbackID: "test/unpriced", unpriced: true},
		{name: "unknown fallback with override", fallbackID: "test/unknown", fallbackCost: &latest.CostConfig{Input: 1, Output: 2}, wantCost: 0.5},
		{name: "same ID without override", fallbackID: "test/primary", wantCost: 0.4},
		{name: "same ID with override", fallbackID: "test/primary", fallbackCost: &latest.CostConfig{Input: 1, Output: 2}, wantCost: 0.5},
		{name: "primary succeeds", fallbackID: "test/fallback", primarySucceeds: true, wantCost: 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store := modelsdev.NewDatabaseStore(&modelsdev.Database{Providers: map[string]modelsdev.Provider{
				"test": {Models: map[string]modelsdev.Model{
					"primary": {Cost: &modelsdev.Cost{Input: 1, Output: 1}},
					"fallback": {Cost: &modelsdev.Cost{
						Input: 2.5, Output: 15,
						Tiers: []modelsdev.CostTier{{
							Rates: modelsdev.Rates{Input: 5, Output: 22.5},
							Tier:  modelsdev.TierSpec{Type: "context", Size: 272_000},
						}},
					}},
					"unpriced": {},
				}},
			}})
			primary := &countingProvider{id: "test/primary", err: errors.New("401 unauthorized")}
			if !tc.primarySucceeds {
				primary.failCount = 100
			}
			fallback := &countingProvider{id: tc.fallbackID}
			root := agent.New("root", "test",
				agent.WithModel(&pricingProvider{Provider: primary, cost: &latest.CostConfig{Input: 10, Output: 10}}),
				agent.WithFallbackModel(&pricingProvider{Provider: fallback, cost: tc.fallbackCost}),
				agent.WithFallbackRetries(-1),
				agent.WithHooks(&latest.HooksConfig{AfterLLMCall: []latest.HookDefinition{{Type: "builtin", Command: "capture-pricing"}}}),
			)
			rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)),
				WithModelStore(store), WithSessionCompaction(false), WithBudget(&latest.BudgetConfig{MaxCost: 100}))
			require.NoError(t, err)
			var captured *hooks.Input
			require.NoError(t, rt.hooksRegistry.RegisterBuiltin("capture-pricing",
				func(_ context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
					snapshot := *in
					captured = &snapshot
					return nil, nil
				}))

			wantID := tc.fallbackID
			if tc.primarySucceeds {
				wantID = primary.id
			}
			sess := session.New(session.WithUserMessage("hi"))
			sess.Title = "Fallback pricing test"
			for turn := range 2 {
				primary.stream = newStreamBuilder().AddContent("ok").AddStopWithUsage(300_000, 100_000).Build()
				fallback.stream = newStreamBuilder().AddContent("ok").AddStopWithUsage(300_000, 100_000).Build()
				captured = nil
				var message *chat.Message
				var usage *MessageUsage
				var budget *BudgetStatus
				for ev := range rt.RunStream(t.Context(), sess) {
					switch ev := ev.(type) {
					case *ErrorEvent:
						t.Errorf("unexpected runtime error: %s", ev.Error)
					case *MessageAddedEvent:
						if ev.Message != nil && ev.Message.Message.Role == chat.MessageRoleAssistant {
							message = &ev.Message.Message
						}
					case *TokenUsageEvent:
						if ev.Usage != nil && ev.Usage.LastMessage != nil {
							usage = ev.Usage.LastMessage
						}
					case *BudgetUsageEvent:
						for _, b := range ev.Budgets {
							if b.Name == runBudgetName {
								budget = &b
							}
						}
					}
				}
				require.NotNil(t, captured)
				assert.Equal(t, wantID, captured.ModelID)
				if tc.unpriced {
					assert.Nil(t, captured.Cost)
				} else {
					require.NotNil(t, captured.Cost)
					assert.InDelta(t, tc.wantCost, *captured.Cost, 1e-9)
				}
				require.NotNil(t, message)
				assert.Equal(t, wantID, message.Model)
				assert.InDelta(t, tc.wantCost, message.Cost, 1e-9)
				require.NotNil(t, usage)
				assert.Equal(t, wantID, usage.Model)
				assert.InDelta(t, tc.wantCost, usage.Cost, 1e-9)
				require.NotNil(t, budget)
				assert.Equal(t, tc.unpriced, budget.Unpriced)
				assert.InDelta(t, float64(turn+1)*tc.wantCost, budget.Cost, 1e-9)
				assert.InDelta(t, float64(turn+1)*tc.wantCost, sess.OwnCost(), 1e-9)
			}
			if tc.primarySucceeds {
				assert.Equal(t, 2, primary.callCount)
				assert.Zero(t, fallback.callCount)
			} else {
				assert.Equal(t, 1, primary.callCount, "cooldown skips the failed primary on the second turn")
				assert.Equal(t, 2, fallback.callCount)
			}
		})
	}
}
