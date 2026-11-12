package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

const apiBudgetModelsYAML = `models:
  offline:
    provider: openai
    model: gpt-4o
    base_url: %s/v1
    provider_opts:
      api_type: openai_chatcompletions
`

func cleanupAPIBudgetRuntimes(t *testing.T, sm *SessionManager) {
	t.Helper()
	t.Cleanup(func() {
		sm.runtimeSessions.Range(func(_ string, rs *activeRuntimes) bool {
			require.NoError(t, rs.runtime.Close())
			return true
		})
	})
}

func runAPIBudgetTurn(t *testing.T, sm *SessionManager, sessionID, selected string) []runtime.Event {
	t.Helper()
	stream, err := sm.RunSession(t.Context(), sessionID, "agent.yaml", selected,
		[]api.Message{{Role: chat.MessageRoleUser, Content: "Reply offline"}}, "")
	require.NoError(t, err)
	var events []runtime.Event
	for event := range stream {
		if failure, ok := event.(*runtime.ErrorEvent); ok {
			t.Errorf("runtime error: %s", failure.Error)
		}
		events = append(events, event)
	}
	return events
}

func lastAPIBudgetUsage(t *testing.T, events []runtime.Event) *runtime.BudgetUsageEvent {
	t.Helper()
	for _, event := range slices.Backward(events) {
		if usage, ok := event.(*runtime.BudgetUsageEvent); ok {
			return usage
		}
	}
	t.Fatal("API stream has no budget usage event")
	return nil
}

func requireAPIBudgetStop(t *testing.T, events []runtime.Event, agentName, budget, limit string) {
	t.Helper()
	var stops []*runtime.BudgetExceededEvent
	for _, event := range events {
		switch e := event.(type) {
		case *runtime.BudgetExceededEvent:
			stops = append(stops, e)
		case *runtime.TokenUsageEvent:
			t.Errorf("blocked turn must not spend: %T", event)
		}
	}
	require.Len(t, stops, 1)
	assert.Equal(t, agentName, stops[0].GetAgentName())
	assert.Equal(t, budget, stops[0].Budget)
	assert.Equal(t, limit, stops[0].Limit)
	require.NotNil(t, stops[0].StopMessage)
	assert.Equal(t, stops[0].Message, stops[0].StopMessage.Message.Content)
}

func TestAPISessionBudget_NamedWalletOnlyChargesDeclaredAgents(t *testing.T) {
	t.Parallel()
	const agentsYAML = `agents:
  root:
    model: offline
    instruction: Reply briefly.
    budgets: [shared]
  sibling:
    model: offline
    instruction: Reply briefly.
budgets:
  shared:
    max_tokens: 200
`
	rc, requests := newAPIRuntimeConfig(t)
	sm := newAPIRuntimeConfigManager(t, rc, agentsYAML+fmt.Sprintf(apiBudgetModelsYAML, rc.ModelsGateway))
	cleanupAPIBudgetRuntimes(t, sm)
	sess, err := sm.CreateSession(t.Context(), session.New(session.WithTitle("Named wallet")))
	require.NoError(t, err)

	first := lastAPIBudgetUsage(t, runAPIBudgetTurn(t, sm, sess.ID, "root"))
	require.Len(t, first.Budgets, 1)
	assert.Equal(t, "shared", first.Budgets[0].Name)
	assert.Equal(t, int64(150), first.Budgets[0].Tokens)
	assert.InDelta(t, 0.0006, first.Budgets[0].Cost, 1e-9)
	cached, ok := sm.runtimeSessions.Load(sess.ID)
	require.True(t, ok)

	second := lastAPIBudgetUsage(t, runAPIBudgetTurn(t, sm, sess.ID, "root"))
	require.Len(t, second.Budgets, 1)
	assert.Equal(t, int64(300), second.Budgets[0].Tokens)
	assert.InDelta(t, 0.0012, second.Budgets[0].Cost, 1e-9)
	require.Len(t, second.Budgets[0].PerAgent, 1)
	assert.Equal(t, "root", second.Budgets[0].PerAgent[0].AgentName)
	assert.Equal(t, int64(300), second.Budgets[0].PerAgent[0].Tokens)
	requireAPIBudgetStop(t, runAPIBudgetTurn(t, sm, sess.ID, "root"), "root", "shared", "max_tokens")
	assert.Len(t, requests, 2, "exhausted cached wallet must block the next provider call")

	require.NoError(t, cached.runtime.SetCurrentAgent(t.Context(), "sibling"))
	events := runAPIBudgetTurn(t, sm, sess.ID, "root")
	for _, event := range events {
		if _, ok := event.(*runtime.BudgetExceededEvent); ok {
			t.Errorf("unbudgeted sibling must not be blocked: %T", event)
		}
	}
	assert.Equal(t, second.Budgets, lastAPIBudgetUsage(t, events).Budgets,
		"an unbudgeted sibling may see the wallet, but must not charge it")
	assert.Equal(t, "sibling", cached.runtime.CurrentAgentName(t.Context()))
	assert.Len(t, requests, 3)
	assert.Equal(t, "Offline reply", cached.session.GetLastAssistantMessageContent())

	require.NoError(t, cached.runtime.SetCurrentAgent(t.Context(), "root"))
	requireAPIBudgetStop(t, runAPIBudgetTurn(t, sm, sess.ID, "root"), "root", "shared", "max_tokens")
	assert.Len(t, requests, 3, "switching away and back must not refill the wallet")
	reused, ok := sm.runtimeSessions.Load(sess.ID)
	require.True(t, ok)
	assert.Same(t, cached, reused, "all turns must reuse the live API runtime")

	independent, err := sm.CreateSession(t.Context(), session.New(session.WithTitle("Unbudgeted sibling")))
	require.NoError(t, err)
	independentUsage := lastAPIBudgetUsage(t, runAPIBudgetTurn(t, sm, independent.ID, "sibling"))
	require.Len(t, independentUsage.Budgets, 1)
	assert.Zero(t, independentUsage.Budgets[0].Tokens)
	assert.Zero(t, independentUsage.Budgets[0].Cost)
	assert.Empty(t, independentUsage.Budgets[0].PerAgent)
	assert.Len(t, requests, 4, "another unbudgeted session must remain independent")
	stored, err := sm.sessionStore.GetSession(t.Context(), independent.ID)
	require.NoError(t, err)
	input, output := stored.Usage()
	assert.Equal(t, int64(100), input)
	assert.Equal(t, int64(50), output)
	assert.InDelta(t, 0.0006, stored.TotalCost(), 1e-9)

	budgeted, err := sm.CreateSession(t.Context(), session.New(session.WithTitle("Independent root")))
	require.NoError(t, err)
	fresh := lastAPIBudgetUsage(t, runAPIBudgetTurn(t, sm, budgeted.ID, "root"))
	require.Len(t, fresh.Budgets, 1)
	assert.Equal(t, int64(150), fresh.Budgets[0].Tokens)
	assert.Equal(t, int64(200), fresh.Budgets[0].MaxTokens)
	assert.Len(t, requests, 5, "a separate budgeted session must receive its own allowance")
}

// Script only the model boundary; keep its loaded configuration for native pricing.
type apiBudgetDelegatingModel struct {
	provider.Provider

	delegate bool
	advance  func()
	calls    atomic.Int32
}

func (m *apiBudgetDelegatingModel) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	call := m.calls.Add(1)
	if m.advance != nil {
		m.advance()
	}
	delta := chat.MessageDelta{Content: "Delegated reply"}
	finish := chat.FinishReasonStop
	if m.delegate && call == 1 {
		delta = chat.MessageDelta{ToolCalls: []tools.ToolCall{{
			ID: "call_child", Type: "function",
			Function: tools.FunctionCall{Name: "transfer_task", Arguments: `{"agent":"child","task":"Reply offline"}`},
		}}}
		finish = chat.FinishReasonToolCalls
	}
	return &apiBudgetStream{responses: []chat.MessageStreamResponse{
		{Choices: []chat.MessageStreamChoice{{Delta: delta}}},
		{Choices: []chat.MessageStreamChoice{{FinishReason: finish}}, Usage: &chat.Usage{InputTokens: 100, OutputTokens: 50}},
	}}, nil
}

type apiBudgetStream struct {
	responses []chat.MessageStreamResponse
}

func (s *apiBudgetStream) Recv() (chat.MessageStreamResponse, error) {
	if len(s.responses) == 0 {
		return chat.MessageStreamResponse{}, io.EOF
	}
	response := s.responses[0]
	s.responses = s.responses[1:]
	return response, nil
}

func (*apiBudgetStream) Close() {}

func TestAPISessionBudget_DelegationSharesRunAndNamedWallets(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		runTokens   int64
		namedTokens int64
		stoppedBy   string
	}{
		{"run ceiling", 200, 1000, "run"},
		{"named ceiling", 1000, 200, "shared"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			const agentsYAML = `agents:
  root:
    model: offline
    instruction: Delegate to child.
    sub_agents: [child]
    budgets: [shared]
  child:
    model: offline
    instruction: Reply briefly.
    budgets: [shared]
budget:
  max_tokens: %d
budgets:
  shared:
    max_tokens: %d
`
			rc, requests := newAPIRuntimeConfig(t)
			sm := newAPIRuntimeConfigManager(t, rc, fmt.Sprintf(agentsYAML, tc.runTokens, tc.namedTokens)+fmt.Sprintf(apiBudgetModelsYAML, rc.ModelsGateway))
			cleanupAPIBudgetRuntimes(t, sm)
			models := make(map[string]*apiBudgetDelegatingModel)
			var builds int
			sm.newRuntime = func(ctx context.Context, tm *team.Team, opts ...runtime.Opt) (runtime.Runtime, error) {
				builds++
				for _, name := range []string{"root", "child"} {
					a, err := tm.Agent(name)
					if err != nil {
						return nil, err
					}
					m := &apiBudgetDelegatingModel{Provider: a.Model(ctx), delegate: name == "root"}
					a.SetModelOverride(m)
					models[name] = m
				}
				return runtime.New(ctx, tm, opts...)
			}
			sess, err := sm.CreateSession(t.Context(), session.New(session.WithTitle("Delegation wallet"), session.WithToolsApproved(true)))
			require.NoError(t, err)
			events := runAPIBudgetTurn(t, sm, sess.ID, "root")
			usage := lastAPIBudgetUsage(t, events)
			require.Len(t, usage.Budgets, 2)
			assert.Equal(t, "run", usage.Budgets[0].Name)
			assert.Equal(t, "shared", usage.Budgets[1].Name)
			assert.Equal(t, tc.runTokens, usage.Budgets[0].MaxTokens)
			assert.Equal(t, tc.namedTokens, usage.Budgets[1].MaxTokens)
			for _, wallet := range usage.Budgets {
				assert.Equal(t, int64(300), wallet.Tokens)
				assert.InDelta(t, 0.0012, wallet.Cost, 1e-9)
				assert.False(t, wallet.Unpriced)
				require.Len(t, wallet.PerAgent, 2)
				assert.Equal(t, []string{"child", "root"}, []string{wallet.PerAgent[0].AgentName, wallet.PerAgent[1].AgentName})
				for _, spend := range wallet.PerAgent {
					assert.Equal(t, int64(150), spend.Tokens)
					assert.InDelta(t, 0.0006, spend.Cost, 1e-9)
				}
			}
			var completed *runtime.SubSessionCompletedEvent
			for _, event := range events {
				if e, ok := event.(*runtime.SubSessionCompletedEvent); ok {
					completed = e
				}
			}
			require.NotNil(t, completed, "transfer_task must run a real child session")
			child, ok := completed.SubSession.(*session.Session)
			require.True(t, ok)
			assert.Equal(t, child.ID, usage.SessionID)
			assert.Equal(t, "child", usage.GetAgentName())
			assert.Equal(t, "Delegated reply", child.GetLastAssistantMessageContent())
			assert.EqualValues(t, 1, models["root"].calls.Load(), "child spend must block root's post-transfer call")
			assert.EqualValues(t, 1, models["child"].calls.Load())

			requireAPIBudgetStop(t, runAPIBudgetTurn(t, sm, sess.ID, "root"), "root", tc.stoppedBy, "max_tokens")
			cached, ok := sm.runtimeSessions.Load(sess.ID)
			require.True(t, ok)
			require.NoError(t, cached.runtime.SetCurrentAgent(t.Context(), "child"))
			requireAPIBudgetStop(t, runAPIBudgetTurn(t, sm, sess.ID, "child"), "child", tc.stoppedBy, "max_tokens")
			assert.Equal(t, 1, builds, "subsequent API turns must not rebuild the wallet")
			assert.EqualValues(t, 1, models["root"].calls.Load())
			assert.EqualValues(t, 1, models["child"].calls.Load())
			assert.Empty(t, requests, "scripted delegation must never use the HTTP provider")
			stored, err := sm.sessionStore.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			assert.InDelta(t, 0.0012, stored.TotalCost(), 1e-9)
		})
	}
}

func TestAPISessionBudget_RebuildResetsWalletButKeepsSessionSpend(t *testing.T) {
	t.Parallel()
	for _, serialized := range []bool{false, true} {
		t.Run(fmt.Sprintf("serialized_snapshot=%t", serialized), func(t *testing.T) {
			t.Parallel()
			const agentsYAML = `agents:
  root:
    model: offline
    instruction: Reply briefly.
    budgets: [shared]
budget:
  max_tokens: 150
  max_cost: 0.0005
budgets:
  shared:
    max_tokens: 150
    max_cost: 0.0005
`
			rc, requests := newAPIRuntimeConfig(t)
			sm := newAPIRuntimeConfigManager(t, rc, agentsYAML+fmt.Sprintf(apiBudgetModelsYAML, rc.ModelsGateway))
			cleanupAPIBudgetRuntimes(t, sm)
			sess, err := sm.CreateSession(t.Context(), session.New(session.WithTitle("Rebuild wallet")))
			require.NoError(t, err)
			runAPIBudgetTurn(t, sm, sess.ID, "root")
			requireAPIBudgetStop(t, runAPIBudgetTurn(t, sm, sess.ID, "root"), "root", "run", "max_cost")
			assert.Len(t, requests, 1)
			stored, err := sm.sessionStore.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			input, output := stored.Usage()
			assert.Equal(t, int64(100), input)
			assert.Equal(t, int64(50), output)
			assert.InDelta(t, 0.0006, stored.TotalCost(), 1e-9)
			history := stored.GetAllMessages()

			store := sm.sessionStore
			if serialized {
				data, err := json.Marshal(stored)
				require.NoError(t, err)
				var restored session.Session
				require.NoError(t, json.Unmarshal(data, &restored))
				store = session.NewInMemorySessionStore()
				t.Cleanup(func() { require.NoError(t, store.Close()) })
				require.NoError(t, store.AddSession(t.Context(), &restored))
			}
			rebuilt := NewSessionManager(t.Context(), sm.Sources, store, 0, rc.Clone())
			cleanupAPIBudgetRuntimes(t, rebuilt)
			before, err := rebuilt.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			restoredHistory := before.GetAllMessages()
			require.Len(t, restoredHistory, len(history))
			for i, message := range history {
				assert.Equal(t, message.AgentName, restoredHistory[i].AgentName)
				assert.Equal(t, message.Message, restoredHistory[i].Message)
			}
			assert.InDelta(t, 0.0006, before.TotalCost(), 1e-9)
			beforeInput, beforeOutput := before.Usage()
			assert.Equal(t, input, beforeInput)
			assert.Equal(t, output, beforeOutput)

			usage := lastAPIBudgetUsage(t, runAPIBudgetTurn(t, rebuilt, sess.ID, "root"))
			require.Len(t, usage.Budgets, 2)
			for _, wallet := range usage.Budgets {
				assert.Equal(t, int64(150), wallet.Tokens, "historical tokens must not seed %s", wallet.Name)
				assert.InDelta(t, 0.0006, wallet.Cost, 1e-9, "historical cost must not seed %s", wallet.Name)
			}
			assert.Len(t, requests, 2, "a rebuilt runtime must have a fresh allowance")
			oldRuntime, ok := sm.runtimeSessions.Load(sess.ID)
			require.True(t, ok)
			newRuntime, ok := rebuilt.runtimeSessions.Load(sess.ID)
			require.True(t, ok)
			assert.NotSame(t, oldRuntime.runtime, newRuntime.runtime)
			requireAPIBudgetStop(t, runAPIBudgetTurn(t, rebuilt, sess.ID, "root"), "root", "run", "max_cost")
			assert.Len(t, requests, 2)
			after, err := store.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			// Usage is the last response; message usage retains cumulative spend.
			afterInput, afterOutput := after.Usage()
			assert.Equal(t, int64(100), afterInput)
			assert.Equal(t, int64(50), afterOutput)
			var totalInput, totalOutput int64
			for _, message := range after.GetAllMessages() {
				if usage := message.Message.Usage; usage != nil {
					totalInput += usage.InputTokens
					totalOutput += usage.OutputTokens
				}
			}
			assert.Equal(t, int64(200), totalInput)
			assert.Equal(t, int64(100), totalOutput)
			assert.InDelta(t, 0.0012, after.TotalCost(), 1e-9, "session accounting must survive the runtime rebuild")
		})
	}
}

func TestAPISessionBudget_ActiveTimeAccumulatesWithoutChargingIdle(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"run", "shared"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rc, _ := newAPIRuntimeConfig(t)
			yaml := "agents:\n  root:\n    model: offline\n    instruction: Reply briefly.\n"
			if name == "run" {
				yaml += "budget:\n  max_time: 3s\n"
			} else {
				yaml += "    budgets: [shared]\nbudgets:\n  shared:\n    max_time: 3s\n"
			}
			sm := newAPIRuntimeConfigManager(t, rc, yaml+fmt.Sprintf(apiBudgetModelsYAML, rc.ModelsGateway))
			cleanupAPIBudgetRuntimes(t, sm)
			var elapsed atomic.Int64
			var model *apiBudgetDelegatingModel
			sm.newRuntime = func(ctx context.Context, tm *team.Team, opts ...runtime.Opt) (runtime.Runtime, error) {
				a, err := tm.Agent("root")
				if err != nil {
					return nil, err
				}
				model = &apiBudgetDelegatingModel{Provider: a.Model(ctx), advance: func() { elapsed.Add(int64(2 * time.Second)) }}
				a.SetModelOverride(model)
				opts = append(opts, runtime.WithClock(func() time.Time {
					return time.Unix(0, elapsed.Load())
				}))
				return runtime.New(ctx, tm, opts...)
			}
			sess, err := sm.CreateSession(t.Context(), session.New(session.WithTitle("Active-time wallet")))
			require.NoError(t, err)
			first := lastAPIBudgetUsage(t, runAPIBudgetTurn(t, sm, sess.ID, "root"))
			require.Len(t, first.Budgets, 1)
			assert.Equal(t, name, first.Budgets[0].Name)
			assert.InDelta(t, 2, first.Budgets[0].ElapsedSeconds, 1e-9)
			assert.InDelta(t, 3, first.Budgets[0].MaxTimeSeconds, 1e-9)
			elapsed.Add(int64(time.Hour))
			second := lastAPIBudgetUsage(t, runAPIBudgetTurn(t, sm, sess.ID, "root"))
			require.Len(t, second.Budgets, 1)
			assert.InDelta(t, 4, second.Budgets[0].ElapsedSeconds, 1e-9, "idle time must not consume the allowance")
			requireAPIBudgetStop(t, runAPIBudgetTurn(t, sm, sess.ID, "root"), "root", name, "max_time")
			assert.EqualValues(t, 2, model.calls.Load())
		})
	}
}
