package runtime

import (
	"context"
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/anthropic"
	"github.com/docker/docker-agent/pkg/model/provider/anthropic/anthropictest"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

func TestServerFallbackCountsAllAttemptsTowardsTokenBudgets(t *testing.T) {
	t.Parallel()

	const primary, fallback = "claude-opus-4-8", "claude-sonnet-4-6"
	reply := anthropictest.FallbackBillingReply("m", false,
		anthropictest.BillingHop{Model: primary, Category: "cyber", Iterations: [][4]int64{{60, 2, 3, 4}}},
		anthropictest.BillingHop{Model: fallback, Iterations: [][4]int64{{10, 1, 5, 6}}})
	server := anthropictest.NewServer(t, reply)
	cfg := &latest.ModelConfig{
		Provider: "anthropic", Model: primary,
		ProviderOpts: map[string]any{"fallbacks": []any{fallback}},
	}
	model, err := anthropic.NewClientFromFactory(t.Context(), cfg, environment.NewMapEnvProvider(nil), func(context.Context) (sdk.Client, error) {
		return sdk.NewClient(option.WithAPIKey("test-key"), option.WithBaseURL(server.URL())), nil
	})
	require.NoError(t, err)
	root := agent.New("root", "test", agent.WithModel(model))
	store := catalogStore{costs: map[string]modelsdev.Cost{
		"anthropic/" + primary:  {Input: 1, Output: 1, CacheRead: 1, CacheWrite: 1},
		"anthropic/" + fallback: {Input: 1, Output: 1, CacheRead: 1, CacheWrite: 1},
	}}
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)),
		WithSessionCompaction(false), WithModelStore(store),
		WithBudget(&latest.BudgetConfig{MaxTokens: 80}),
		WithNamedBudgets(map[string]latest.BudgetConfig{"work": {MaxTokens: 80}}, map[string][]string{"root": {"work"}}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	sess := session.New(session.WithUserMessage("q"), session.WithTitle("Fallback token budget"))
	events := drainRunStream(t, rt, sess)
	requireNoErrorEvents(t, events)
	require.Len(t, server.Requests(), 1)
	for _, name := range []string{runBudgetName, "work"} {
		snapshot := rt.currentBudget().trackers[name].snapshot()
		assert.Equal(t, int64(91), snapshot.Tokens)
		require.Len(t, snapshot.PerAgent, 1)
		assert.Equal(t, int64(91), snapshot.PerAgent[0].Tokens)
		assert.InDelta(t, 91.0/1e6, snapshot.Cost, 1e-12)
	}
	input, output := sess.Usage()
	assert.Equal(t, int64(21), input)
	assert.Equal(t, int64(1), output)

	sess.AddMessage(session.UserMessage("another request"))
	events = drainRunStream(t, rt, sess)
	requireNoErrorEvents(t, events)
	assert.Len(t, server.Requests(), 1, "exhausted budgets must prevent the next model request")
	var exceeded bool
	for _, event := range events {
		if event, ok := event.(*BudgetExceededEvent); ok {
			assert.Equal(t, string(budgetLimitTokens), event.Limit)
			exceeded = true
		}
	}
	assert.True(t, exceeded)
}
