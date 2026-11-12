package runtime

import (
	"context"
	"path/filepath"
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/model/provider/anthropic"
	"github.com/docker/docker-agent/pkg/model/provider/anthropic/anthropictest"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

func TestFallbackBillingRuntimeEvidence(t *testing.T) {
	t.Parallel()
	const primary, resolved, fallback = "claude-opus-4-8", "claude-opus-4-8-20260901", "claude-sonnet-4-6"
	for name, tc := range map[string]struct {
		reply string
		cost  float64
	}{
		"all primary server tool iterations use override": {
			reply: anthropictest.FallbackBillingReply("m", false,
				anthropictest.BillingHop{Model: resolved, From: primary, Category: "cyber", Iterations: [][4]int64{{10, 7, 3, 4}, {20, 0, 5, 6}}},
				anthropictest.BillingHop{Model: fallback, Iterations: [][4]int64{{30, 2, 4, 5}}}),
			// Primary override: 30*1 + 7*2 + 8*3 + 10*4 = 108.
			// Fallback catalogue: 30*10 + 2*20 + 4*30 + 5*40 = 660.
			cost: 768.0 / 1e6,
		},
		"all billed before output primary iterations use override": {
			reply: anthropictest.FallbackBillingReply("m", false,
				anthropictest.BillingHop{Model: resolved, From: primary, Category: "bio", Iterations: [][4]int64{{10, 0, 3, 4}, {20, 0, 5, 6}}},
				anthropictest.BillingHop{Model: fallback, Iterations: [][4]int64{{30, 2, 4, 5}}}),
			cost: 754.0 / 1e6,
		},
		"null before output is unbilled": {
			reply: anthropictest.FallbackBillingReply("m", false,
				anthropictest.BillingHop{Model: resolved, From: primary, Iterations: [][4]int64{{10, 0, 3, 4}}},
				anthropictest.BillingHop{Model: fallback, Iterations: [][4]int64{{30, 2, 4, 5}}}),
			cost: 660.0 / 1e6,
		},
		"missing category remains billed": {
			reply: anthropictest.FallbackBillingReply("m", false,
				anthropictest.BillingHop{Model: resolved, From: primary, MissingCategory: true, Iterations: [][4]int64{{10, 0, 3, 4}}},
				anthropictest.BillingHop{Model: fallback, Iterations: [][4]int64{{30, 2, 4, 5}}}),
			cost: 695.0 / 1e6,
		},
		"exhausted chain with null terminal costs zero": {
			reply: anthropictest.FallbackBillingReply("m", true,
				anthropictest.BillingHop{Model: resolved, From: primary, Category: "cyber", Iterations: [][4]int64{{10, 0, 3, 4}}},
				anthropictest.BillingHop{Model: fallback, Iterations: [][4]int64{{30, 0, 4, 5}}}),
		},
		"exhausted chain bio terminal is billed": {
			reply: anthropictest.FallbackBillingReply("m", true,
				anthropictest.BillingHop{Model: resolved, From: primary, Category: "cyber", Iterations: [][4]int64{{10, 0, 3, 4}}},
				anthropictest.BillingHop{Model: fallback, Category: "bio", Iterations: [][4]int64{{30, 0, 4, 5}}}),
			cost: 620.0 / 1e6,
		},
		"sticky tool loop never uses primary override": {
			reply: anthropictest.FallbackBillingReply("m", false,
				anthropictest.BillingHop{Model: fallback, Iterations: [][4]int64{{10, 0, 3, 4}, {20, 7, 5, 6}}}),
			cost: 1080.0 / 1e6,
		},
		"sticky fallback never uses primary override": {
			reply: anthropictest.StickyReply("m", fallback, "hi"),
			cost:  490.0 / 1e6,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server := anthropictest.NewServer(t, tc.reply)
			cfg := latest.ModelConfig{
				Provider: "anthropic", Model: primary, DisplayModel: "opus-alias",
				Cost:         &latest.CostConfig{Input: 1, Output: 2, CacheRead: 3, CacheWrite: 4},
				ProviderOpts: map[string]any{"fallbacks": []any{fallback}},
			}
			model, err := anthropic.NewClientFromFactory(t.Context(), &cfg, environment.NewMapEnvProvider(nil), func(context.Context) (sdk.Client, error) {
				return sdk.NewClient(option.WithAPIKey("test-key"), option.WithBaseURL(server.URL())), nil
			})
			require.NoError(t, err)
			root := agent.New("root", "test", agent.WithModel(model),
				agent.WithHooks(&latest.HooksConfig{AfterLLMCall: []latest.HookDefinition{{Type: "builtin", Command: "capture-hop-cost"}}}))
			store := openSessionStore(t, filepath.Join(t.TempDir(), "sessions.db"))
			models := catalogStore{costs: map[string]modelsdev.Cost{
				"anthropic/opus-alias":  {Input: 1000, Output: 2000, CacheRead: 3000, CacheWrite: 4000},
				"anthropic/" + resolved: {Input: 1000, Output: 2000, CacheRead: 3000, CacheWrite: 4000},
				"anthropic/" + fallback: {Input: 10, Output: 20, CacheRead: 30, CacheWrite: 40},
			}}
			rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithSessionCompaction(false), WithSessionStore(store), WithModelStore(models), WithBudget(&latest.BudgetConfig{MaxCost: 1}))
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, rt.Close()) })
			var hookCost *float64
			require.NoError(t, rt.hooksRegistry.RegisterBuiltin("capture-hop-cost", func(_ context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
				hookCost = in.Cost
				return nil, nil
			}))
			sess := session.New(session.WithUserMessage("q"), session.WithTitle("Fallback billing"))
			events := drainRunStream(t, rt, sess)
			requireNoErrorEvents(t, events)
			require.NotNil(t, hookCost)
			assert.InDelta(t, tc.cost, *hookCost, 1e-12)
			assert.InDelta(t, tc.cost, sess.TotalCost(), 1e-12)
			assert.InDelta(t, tc.cost, rt.currentBudget().trackers[runBudgetName].snapshot().Cost, 1e-12)
			var usage *MessageUsage
			for _, ev := range events {
				if ev, ok := ev.(*TokenUsageEvent); ok && ev.Usage.LastMessage != nil {
					usage = ev.Usage.LastMessage
				}
			}
			require.NotNil(t, usage)
			assert.Equal(t, "anthropic/"+fallback, usage.Model)
			assert.InDelta(t, tc.cost, usage.Cost, 1e-12)
			loaded, err := store.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			var messages []chat.Message
			for _, item := range loaded.Messages {
				if item.Message != nil && item.Message.Message.Role == chat.MessageRoleAssistant {
					messages = append(messages, item.Message.Message)
				}
			}
			require.Len(t, messages, 1)
			assert.Equal(t, "anthropic/"+fallback, messages[0].Model)
			assert.InDelta(t, tc.cost, messages[0].Cost, 1e-12)
			assert.Equal(t, usage.Usage, *messages[0].Usage)
		})
	}
}
