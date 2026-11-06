package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/model/provider/rulebased"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/team"
)

type routingPricingProvider struct {
	*countingProvider

	config base.Config
}

func (p *routingPricingProvider) BaseConfig() base.Config { return p.config }

func TestRoutedPricing(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name            string
		routerID        string
		leafID          string
		routerCost      *latest.CostConfig
		leafCost        *latest.CostConfig
		routerURL       string
		leafURL         string
		resolvedLeafURL string
		tier            string
		noMatch         bool
		fallback        bool
		outerRouter     bool
		want            float64
		unpriced        bool
		reload          bool
	}{
		{name: "catalog leaf", want: 0.43},
		{name: "catalog leaf survives reload", reload: true, want: 0.43},
		{name: "unknown router", routerID: "local/router", want: 0.43},
		{name: "router override does not leak", routerCost: &latest.CostConfig{Input: 99, Output: 99}, want: 0.43},
		{name: "leaf override and cache classes", leafCost: &latest.CostConfig{Input: 1, Output: 2, CacheRead: 0.1, CacheWrite: 1.25}, want: 0.0995},
		{name: "free leaf", leafCost: &latest.CostConfig{}, want: 0},
		{name: "same ID response tier", routerID: "openai/gpt-6-astra", leafID: "openai/gpt-6-astra", tier: "ultrafast", want: 2.58},
		{name: "response downgrade", leafID: "openai/gpt-6-astra", tier: "default", want: 0.43},
		{name: "configured leaf endpoint", leafID: "openai/gpt-6-astra", leafURL: "https://leaf.example/v1", tier: "ultrafast", want: 0.43},
		{name: "resolved leaf endpoint", leafID: "openai/gpt-6-astra", resolvedLeafURL: "https://leaf.example/v1", tier: "ultrafast", want: 0.43},
		{name: "router endpoint does not suppress leaf tier", leafID: "openai/gpt-6-astra", routerURL: "https://router.example/v1", tier: "ultrafast", want: 2.58},
		{name: "unknown custom leaf", leafID: "local/unknown", leafURL: "https://leaf.example/v1", unpriced: true},
		{name: "unknown custom leaf override", leafID: "local/unknown", leafURL: "https://leaf.example/v1", leafCost: &latest.CostConfig{Input: 1, Output: 2, CacheRead: 0.1, CacheWrite: 1.25}, want: 0.0995},
		{name: "no match", noMatch: true, want: 0.072},
		{name: "no match override", noMatch: true, routerCost: &latest.CostConfig{Input: 1, Output: 2, CacheRead: 0.1, CacheWrite: 1.25}, want: 0.0995},
		{name: "no match tier", noMatch: true, routerID: "openai/gpt-6-astra", tier: "ultrafast", want: 2.58},
		{name: "no match declared endpoint", noMatch: true, routerID: "openai/gpt-6-astra", routerURL: "https://router.example/v1", tier: "ultrafast", want: 0.43},
		{name: "no match resolved endpoint", noMatch: true, routerID: "openai/gpt-6-astra", resolvedLeafURL: "https://leaf.example/v1", tier: "ultrafast", want: 0.43},
		{name: "failed route outer fallback and cooldown", fallback: true, tier: "ultrafast", want: 2.58},
		{name: "outer fallback router no match and cooldown", outerRouter: true, noMatch: true, want: 0.072},
		{name: "outer fallback router matched and cooldown", outerRouter: true, want: 0.43},
		{name: "fallback router serves primary ID", outerRouter: true, leafID: "test/failed", want: 0.43},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := modelsdev.NewDatabaseStore(&modelsdev.Database{Providers: map[string]modelsdev.Provider{
				"test": {Models: map[string]modelsdev.Model{
					"failed": {Name: "Leaf", Cost: &modelsdev.Cost{Input: 4, Output: 8, CacheRead: 0.5, CacheWrite: 6}},
				}},
				"openai": {Models: map[string]modelsdev.Model{
					"router":      {Name: "Router", Cost: &modelsdev.Cost{Input: 1, Output: 1, CacheRead: 0.1, CacheWrite: 0.5}},
					"gpt-6-astra": {Name: "Astra", Cost: &modelsdev.Cost{Input: 4, Output: 8, CacheRead: 0.5, CacheWrite: 6}},
				}},
				"anthropic": {Models: map[string]modelsdev.Model{
					"leaf": {Name: "Leaf", Cost: &modelsdev.Cost{Input: 4, Output: 8, CacheRead: 0.5, CacheWrite: 6}},
				}},
			}})
			routerID := modelsdev.ParseIDOrZero(tc.routerID)
			if routerID.IsZero() {
				routerID = modelsdev.NewID("openai", "router")
			}
			leafID := modelsdev.ParseIDOrZero(tc.leafID)
			if leafID.IsZero() {
				leafID = modelsdev.NewID("anthropic", "leaf")
			}
			resolvedURL := tc.resolvedLeafURL
			if resolvedURL == "" {
				resolvedURL = "https://api.openai.com/v1"
			}
			leaf := &routingPricingProvider{countingProvider: &countingProvider{id: leafID.String()}, config: base.Config{
				ModelConfig: latest.ModelConfig{Provider: leafID.Provider, Model: leafID.Model, Cost: tc.leafCost, BaseURL: tc.leafURL, ProviderOpts: map[string]any{"service_tier": "fast"}}, BaseURL: resolvedURL,
			}}
			noMatch := &routingPricingProvider{countingProvider: &countingProvider{id: routerID.String()}, config: base.Config{BaseURL: resolvedURL}}
			fallback := &routingPricingProvider{countingProvider: &countingProvider{id: "openai/gpt-6-astra"}, config: base.Config{BaseURL: "https://api.openai.com/v1"}}
			router, err := rulebased.NewClient(t.Context(), &latest.ModelConfig{
				Provider: routerID.Provider, Model: routerID.Model, Cost: tc.routerCost, BaseURL: tc.routerURL,
				Routing: []latest.RoutingRule{{Model: "leaf", Examples: []string{"poetry"}}},
			}, nil, nil, func(_ context.Context, spec string, _ map[string]latest.ModelConfig, _ environment.Provider, _ ...options.Opt) (rulebased.Provider, error) {
				if spec == "leaf" {
					return leaf, nil
				}
				return noMatch, nil
			})
			require.NoError(t, err)
			model := rulebased.Provider(router)
			primary := &countingProvider{id: "test/failed", failCount: 100, err: errors.New("401 unauthorized")}
			if tc.outerRouter {
				model = primary
			}
			opts := []agent.Opt{agent.WithModel(model), agent.WithHooks(&latest.HooksConfig{AfterLLMCall: []latest.HookDefinition{{Type: "builtin", Command: "capture-routing"}}})}
			if tc.fallback {
				leaf.failCount, leaf.err = 100, errors.New("401 unauthorized")
				opts = append(opts, agent.WithFallbackModel(fallback), agent.WithFallbackRetries(-1))
			}
			if tc.outerRouter {
				opts = append(opts, agent.WithFallbackModel(router), agent.WithFallbackRetries(-1))
			}
			root := agent.New("root", "test", opts...)
			rec := &recordingTelemetry{}
			rtOpts := []Opt{WithTelemetry(rec), WithModelStore(store), WithSessionCompaction(false), WithBudget(&latest.BudgetConfig{MaxCost: 100})}
			dbPath := filepath.Join(t.TempDir(), "sessions.db")
			if tc.reload {
				sessions, err := sqlitestore.New(t.Context(), dbPath)
				require.NoError(t, err)
				t.Cleanup(func() { assert.NoError(t, sessions.Close()) })
				rtOpts = append(rtOpts, WithSessionStore(sessions))
			}
			rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), rtOpts...)
			require.NoError(t, err)
			var captured *hooks.Input
			require.NoError(t, rt.hooksRegistry.RegisterBuiltin("capture-routing", func(_ context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
				snapshot := *in
				captured = &snapshot
				return nil, nil
			}))
			prompt, wantID, wantName := "poetry", leafID.String(), "Leaf"
			switch leafID.Provider {
			case "openai":
				wantName = "Astra"
			case "local":
				wantName = "unknown"
				if tc.leafCost != nil {
					wantName = leafID.Model
				}
			}
			if tc.noMatch {
				prompt, wantID, wantName = "unrelated", routerID.String(), "Router"
				if routerID.Model == "gpt-6-astra" {
					wantName = "Astra"
				}
			} else if tc.fallback {
				wantID, wantName = fallback.id, "Astra"
			}
			sess := session.New(session.WithTitle("Routing pricing test"), session.WithUserMessage(prompt))
			for turn := range 2 {
				for _, p := range []*routingPricingProvider{leaf, noMatch, fallback} {
					stream := newStreamBuilder().AddContent("ok").AddStopWithUsage(0, 0).Build()
					stream.responses[len(stream.responses)-1].Usage = &chat.Usage{InputTokens: 50_000, CachedInputTokens: 20_000, CacheWriteTokens: 30_000, OutputTokens: 5_000, ServiceTier: tc.tier}
					p.stream = stream
				}
				captured = nil
				var sidebarModel string
				var sidebarModels []string
				var message *chat.Message
				var usage *MessageUsage
				var budget *BudgetStatus
				for ev := range rt.RunStream(t.Context(), sess) {
					switch ev := ev.(type) {
					case *AgentInfoEvent:
						sidebarModels = append(sidebarModels, ev.Model)
						sidebarModel = ev.Model
					case *ModelFallbackEvent:
						sidebarModel = ev.FallbackModel
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
				require.NotEmpty(t, sidebarModels)
				assert.Equal(t, wantID, sidebarModels[len(sidebarModels)-1], "AgentInfo-only consumers also receive the serving identity")
				assert.Equal(t, wantID, sidebarModel, "both AgentInfo and ModelFallback update the sidebar")
				if wantID != model.ID().String() && (!tc.fallback && !tc.outerRouter || turn > 0) {
					assert.Equal(t, []string{model.ID().String(), wantID}, sidebarModels)
				}
				if tc.unpriced {
					assert.Nil(t, captured.Cost)
				} else {
					require.NotNil(t, captured.Cost)
					assert.InDelta(t, tc.want, *captured.Cost, 1e-9)
				}
				require.NotNil(t, message)
				assert.Equal(t, wantID, message.Model)
				assert.InDelta(t, tc.want, message.Cost, 1e-9)
				require.NotNil(t, usage)
				assert.Equal(t, wantID, usage.Model)
				assert.InDelta(t, tc.want, usage.Cost, 1e-9)
				require.NotNil(t, budget)
				assert.Equal(t, tc.unpriced, budget.Unpriced)
				assert.InDelta(t, float64(turn+1)*tc.want, budget.Cost, 1e-9)
				assert.InDelta(t, float64(turn+1)*tc.want, sess.OwnCost(), 1e-9)
				records := rec.snapshot().tokenUsages
				require.Len(t, records, turn+1)
				assert.Equal(t, wantName, records[turn].Model)
				assert.InDelta(t, tc.want, records[turn].Cost, 1e-9)
				assert.Equal(t, int64(100_000), records[turn].InputTokens)
				assert.Equal(t, int64(5_000), records[turn].OutputTokens)
			}
			if tc.reload {
				reopened, err := sqlitestore.New(t.Context(), dbPath)
				require.NoError(t, err)
				t.Cleanup(func() { assert.NoError(t, reopened.Close()) })
				loaded, err := reopened.GetSession(t.Context(), sess.ID)
				require.NoError(t, err)
				var assistants int
				for _, m := range loaded.OwnMessages() {
					if m.Message.Role != chat.MessageRoleAssistant {
						continue
					}
					assistants++
					assert.Equal(t, wantID, m.Message.Model, "the serving model, not the router, is persisted")
					assert.InDelta(t, tc.want, m.Message.Cost, 1e-9)
				}
				assert.Equal(t, 2, assistants)
				assert.InDelta(t, 2*tc.want, loaded.OwnCost(), 1e-9)
			}
			if tc.outerRouter {
				assert.Equal(t, 1, primary.callCount, "cooldown skips failed primary before successful fallback router")
			}
			switch {
			case tc.fallback:
				assert.Equal(t, 1, leaf.callCount, "cooldown stays keyed to the failed outer router")
				assert.Equal(t, 2, fallback.callCount)
			case tc.noMatch:
				assert.Equal(t, 2, noMatch.callCount)
				assert.Zero(t, leaf.callCount)
			default:
				assert.Equal(t, 2, leaf.callCount, "successful router is not cooled down")
				assert.Zero(t, noMatch.callCount)
			}
		})
	}
}

func TestServingModelFromPlainStream(t *testing.T) {
	t.Parallel()
	assert.Nil(t, servingModelFrom(newStreamBuilder().Build()))
}

type pricingServingStream struct {
	chat.MessageStream

	serving provider.ServingModel
}

func (s *pricingServingStream) ServingModel() provider.ServingModel { return s.serving }

type pricingErrorStream struct{ err error }

func (s *pricingErrorStream) Recv() (chat.MessageStreamResponse, error) {
	return chat.MessageStreamResponse{}, s.err
}
func (*pricingErrorStream) Close() {}

func TestRoutedIdleRetryServingModel(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, secondModel string
		plain, ordinary   bool
	}{
		{name: "different leaf", secondModel: "second"},
		{name: "dispatch model", secondModel: "router"},
		{name: "plain stream", secondModel: "router", plain: true},
		{name: "ordinary retry dispatch model", secondModel: "router", ordinary: true},
		{name: "ordinary retry plain stream", secondModel: "router", ordinary: true, plain: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				first := provider.ServingModel{ID: modelsdev.NewID("test", "first")}
				second := provider.ServingModel{ID: modelsdev.NewID("test", tc.secondModel), Config: base.Config{BaseURL: "https://second.example/v1"}}
				var recovered chat.MessageStream = newStreamBuilder().AddContent("recovered").AddStopWithUsage(1, 1).Build()
				if !tc.plain {
					recovered = &pricingServingStream{MessageStream: recovered, serving: second}
				}
				var failed chat.MessageStream = &idleErrorStream{}
				retries := -1
				allowance := &idleStreamRetryAllowance{remaining: true, parentSessionID: "parent"}
				if tc.ordinary {
					failed = &pricingErrorStream{err: errors.New("500 internal server error")}
					retries = 1
					allowance = nil
				}
				model := &idleThenResultProvider{id: "test/router", streams: []chat.MessageStream{
					&pricingServingStream{MessageStream: failed, serving: first}, recovered,
				}}
				a := agent.New("delegate", "test", agent.WithModel(model), agent.WithFallbackRetries(retries))
				executor := newFallbackExecutor()
				executor.cooldowns = newCooldownManager(time.Now)
				sink := &collectSink{}
				result, used, err := executor.execute(t.Context(), a, model, nil, nil, session.New(), sink,
					allowance, func() error { return nil })
				require.NoError(t, err)
				assert.Equal(t, model, used)
				if tc.plain {
					assert.Nil(t, result.Serving)
				} else {
					require.NotNil(t, result.Serving)
					assert.Equal(t, second, *result.Serving)
				}
				var announced []string
				for _, event := range sink.events {
					if event, ok := event.(*AgentInfoEvent); ok {
						announced = append(announced, event.Model)
					}
				}
				assert.Equal(t, []string{first.ID.String(), second.ID.String()}, announced)
			})
		})
	}
}

func TestRoutedSameIDFallbackServingModel(t *testing.T) {
	t.Parallel()

	first := provider.ServingModel{ID: modelsdev.NewID("test", "leaf")}
	primary := &idleThenResultProvider{id: "test/router", streams: []chat.MessageStream{
		&pricingServingStream{MessageStream: &pricingErrorStream{err: errors.New("401 unauthorized")}, serving: first},
	}}
	fallback := &routingPricingProvider{countingProvider: &countingProvider{
		id: "test/router", stream: newStreamBuilder().AddContent("ok").AddStopWithUsage(50_000, 5_000).Build(),
	}, config: base.Config{ModelConfig: latest.ModelConfig{Cost: &latest.CostConfig{Input: 2, Output: 3}}}}
	a := agent.New("root", "test", agent.WithModel(primary), agent.WithFallbackModel(fallback), agent.WithFallbackRetries(-1))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(a)), WithModelStore(modelsdev.NewDatabaseStore(&modelsdev.Database{})), WithSessionCompaction(false))
	require.NoError(t, err)
	sess := session.New(session.WithTitle("Same ID fallback"), session.WithUserMessage("hi"))
	var announced []string
	var assistant *chat.Message
	for event := range rt.RunStream(t.Context(), sess) {
		switch event := event.(type) {
		case *ErrorEvent:
			t.Errorf("runtime error: %s", event.Error)
		case *AgentInfoEvent:
			announced = append(announced, event.Model)
		case *ModelFallbackEvent:
			announced = append(announced, event.FallbackModel)
		case *MessageAddedEvent:
			if event.Message != nil && event.Message.Message.Role == chat.MessageRoleAssistant {
				assistant = &event.Message.Message
			}
		}
	}
	assert.Equal(t, []string{"test/router", "test/leaf", "test/router", "test/router"}, announced)
	require.NotNil(t, assistant)
	assert.Equal(t, "test/router", assistant.Model)
	assert.InDelta(t, 0.115, assistant.Cost, 1e-9)
}
