package runtime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

func TestCompactionSharesBudgets(t *testing.T) {
	t.Parallel()

	for _, runWide := range []bool{false, true} {
		name := "named only"
		if runWide {
			name = "run and named"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			primary := &queueProvider{id: "test/primary", streams: []chat.MessageStream{
				newStreamBuilder().AddContent("reply").AddStopWithUsage(100, 50).Build(),
			}}
			summarizer := &queueProvider{id: "test/summary", streams: []chat.MessageStream{
				newStreamBuilder().AddContent("first summary").AddStopWithUsage(100, 50).Build(),
				newStreamBuilder().AddContent("second summary").AddStopWithUsage(100, 50).Build(),
			}}
			worker := agent.New("worker", "test", agent.WithModel(primary), agent.WithCompactionModel(summarizer))
			opts := []Opt{
				WithSessionCompaction(false),
				WithModelStore(mockModelStoreWithCostAndLimit{limit: 100_000, cost: modelsdev.Cost{Input: 10, Output: 20}}),
				WithNamedBudgets(map[string]latest.BudgetConfig{
					"work": {MaxCost: 1}, "other": {MaxCost: 1},
				}, map[string][]string{"worker": {"work"}, "root": {"other"}}),
			}
			if runWide {
				opts = append(opts, WithBudget(&latest.BudgetConfig{MaxCost: 1}))
			}
			rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(worker)), opts...)
			require.NoError(t, err)
			sess := twoMessageSession()
			sess.Title = "Budget test"

			// Manual compaction can be the first operation on a restored session.
			for i := range 2 {
				sink := &collectSink{}
				rt.Summarize(t.Context(), sess, "", sink)
				require.NotNil(t, rt.currentBudget())
				wantCost := float64(i+1) * 0.002
				assert.InDelta(t, wantCost, sess.TotalCost(), 1e-9)
				usages := sink.budgetUsages()
				require.NotEmpty(t, usages)
				for _, event := range usages {
					assert.Equal(t, sess.ID, event.SessionID)
					assert.Equal(t, "worker", event.AgentName)
				}
				for _, budget := range usages[len(usages)-1].Budgets {
					if budget.Name == "other" {
						assert.Zero(t, budget.Cost)
						assert.Empty(t, budget.PerAgent)
						continue
					}
					assert.InDelta(t, wantCost, budget.Cost, 1e-9)
					assert.Equal(t, int64((i+1)*150), budget.Tokens)
					require.Len(t, budget.PerAgent, 1)
					assert.Equal(t, "worker", budget.PerAgent[0].AgentName)
					assert.InDelta(t, wantCost, budget.PerAgent[0].Cost, 1e-9)
				}
			}

			shared := rt.currentBudget()
			for event := range rt.RunStream(t.Context(), sess) {
				if e, ok := event.(*ErrorEvent); ok {
					t.Errorf("runtime error: %s", e.Error)
				}
			}
			assert.Same(t, shared, rt.currentBudget())
			assert.InDelta(t, 0.006, shared.trackers["work"].snapshot().Cost, 1e-9)
			assert.InDelta(t, 0.006, sess.TotalCost(), 1e-9)
			assert.Empty(t, primary.streams)
			assert.Empty(t, summarizer.streams)
		})
	}
}

func TestCompactionSubSessionSharesTimeBudget(t *testing.T) {
	t.Parallel()

	var elapsed atomic.Int64
	prov := &queueProvider{id: "test/model", streams: []chat.MessageStream{&hookStream{
		mockStream: newStreamBuilder().AddContent("summary").AddStopWithUsage(100, 50).Build(),
		onStop:     func() { elapsed.Store(int64(3 * time.Second)) },
	}}}
	root := agent.New("root", "test", agent.WithModel(prov))
	worker := agent.New("worker", "test", agent.WithModel(prov))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root, worker)),
		WithClock(func() time.Time { return budgetEpoch.Add(time.Duration(elapsed.Load())) }),
		WithBudget(&latest.BudgetConfig{MaxCost: 1}),
		WithNamedBudgets(map[string]latest.BudgetConfig{"work": {MaxTime: latest.Duration{Duration: 2 * time.Second}}},
			map[string][]string{"worker": {"work"}}),
		WithModelStore(mockModelStoreWithCostAndLimit{limit: 100_000, cost: modelsdev.Cost{Input: 10, Output: 20}}))
	require.NoError(t, err)
	sess := twoMessageSession()
	sess.AgentName = "worker"
	sess.ParentID = session.New().ID
	sink := &collectSink{}
	rt.Summarize(t.Context(), sess, "", sink)

	usages := sink.budgetUsages()
	require.NotEmpty(t, usages)
	last := usages[len(usages)-1]
	assert.Equal(t, sess.ID, last.SessionID)
	assert.Equal(t, "worker", last.AgentName)
	for _, budget := range last.Budgets {
		assert.InDelta(t, 3, budget.ElapsedSeconds, 1e-9)
		require.Len(t, budget.PerAgent, 1)
		assert.Equal(t, "worker", budget.PerAgent[0].AgentName)
		assert.InDelta(t, 3, budget.PerAgent[0].ActiveSeconds, 1e-9)
	}
	breach := rt.currentBudget().exceededFor("worker")
	require.NotNil(t, breach)
	assert.Equal(t, "budgets.work.max_time", breach.configPath())
	assert.Nil(t, rt.currentBudget().exceededFor("root"))
}

func TestCompactionBudgetStopsNextModelCall(t *testing.T) {
	t.Parallel()

	for _, automatic := range []bool{false, true} {
		name := "manual"
		if automatic {
			name = "automatic"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			prov := &queueProvider{id: "test/model", streams: []chat.MessageStream{
				newStreamBuilder().AddContent("reply").AddStopWithUsage(100, 50).Build(),
				newStreamBuilder().AddContent("the summary").AddStopWithUsage(100, 50).Build(),
				newStreamBuilder().AddContent("must not run").AddStopWithUsage(100, 50).Build(),
			}}
			root := agent.New("root", "test", agent.WithModel(prov))
			rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)),
				WithSessionCompaction(automatic), WithBudget(&latest.BudgetConfig{MaxCost: 0.003}),
				WithModelStore(mockModelStoreWithCostAndLimit{limit: 100_000, cost: modelsdev.Cost{Input: 10, Output: 20}}))
			require.NoError(t, err)
			sess := session.New(session.WithTitle("Budget test"), session.WithUserMessage("hi"))
			for range rt.RunStream(t.Context(), sess) {
			}
			if automatic {
				sess.SetUsage(95_000, 0)
			} else {
				rt.Summarize(t.Context(), sess, "", &collectSink{})
			}
			var stops []*BudgetExceededEvent
			for event := range rt.RunStream(t.Context(), sess) {
				switch e := event.(type) {
				case *BudgetExceededEvent:
					stops = append(stops, e)
				case *ErrorEvent:
					t.Errorf("runtime error: %s", e.Error)
				}
			}
			require.Len(t, stops, 1)
			assert.Equal(t, sess.ID, stops[0].SessionID)
			assert.Equal(t, "budget.max_cost", stops[0].ConfigPath)
			assert.InDelta(t, 0.004, sess.TotalCost(), 1e-9)
			assert.InDelta(t, sess.TotalCost(), rt.currentBudget().trackers[runBudgetName].snapshot().Cost, 1e-9)
			assert.Len(t, prov.streams, 1, "compaction must exhaust the budget before the next model call")
		})
	}
}

func TestCompactionExhaustedBudgetPreservesHistory(t *testing.T) {
	t.Parallel()

	prov := &queueProvider{id: "test/model", streams: []chat.MessageStream{
		newStreamBuilder().AddContent("must not run").AddStopWithUsage(100, 50).Build(),
	}}
	worker := agent.New("worker", "test", agent.WithModel(prov))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(worker)),
		WithNamedBudgets(map[string]latest.BudgetConfig{"work": {MaxCost: 0.001}}, map[string][]string{"worker": {"work"}}),
		WithModelStore(mockModelStoreWithCostAndLimit{limit: 100_000, cost: modelsdev.Cost{Input: 10, Output: 20}}))
	require.NoError(t, err)
	rt.ensureBudget()
	rt.recordBudget(session.New(), worker, nil, new(0.001), 0, &collectSink{})
	sess := twoMessageSession()
	before := sess.MessagesSnapshot()
	sink := &collectSink{}
	rt.Summarize(t.Context(), sess, "", sink)

	assert.Equal(t, before, sess.MessagesSnapshot(), "a budget stop must never become the summary")
	assert.Len(t, prov.streams, 1)
	var skipped, warned bool
	for _, event := range sink.events {
		switch e := event.(type) {
		case *SessionCompactionEvent:
			if e.Status == "completed" {
				assert.Equal(t, CompactionOutcomeSkipped, e.Outcome)
				skipped = true
			}
		case *WarningEvent:
			assert.Contains(t, e.Message, "budgets.work.max_cost")
			warned = true
		case *ErrorEvent, *SessionSummaryEvent, *BudgetExceededEvent:
			t.Errorf("unexpected event: %T", event)
		}
	}
	assert.True(t, skipped)
	assert.True(t, warned)
}

func TestCompactionBudgetRecordsUnappliedSummaries(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		content     string
		storeError  bool
		unpriced    bool
		wantOutcome string
	}{
		{name: "empty summary", wantOutcome: CompactionOutcomeSkipped},
		{name: "persistence failure", content: "summary", storeError: true, wantOutcome: CompactionOutcomeFailed},
		{name: "unpriced summary", content: "summary", unpriced: true, wantOutcome: CompactionOutcomeApplied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			prov := &mockProvider{id: "test/model", stream: newStreamBuilder().AddContent(tc.content).AddStopWithUsage(100, 50).Build()}
			root := agent.New("root", "test", agent.WithModel(prov))
			var cost *modelsdev.Cost
			if !tc.unpriced {
				cost = &modelsdev.Cost{Input: 10, Output: 20}
			}
			store := modelsdev.NewDatabaseStore(&modelsdev.Database{Providers: map[string]modelsdev.Provider{
				"test": {Models: map[string]modelsdev.Model{"model": {Limit: modelsdev.Limit{Context: 100_000}, Cost: cost}}},
			}})
			opts := []Opt{WithBudget(&latest.BudgetConfig{MaxCost: 1}), WithModelStore(store)}
			if tc.storeError {
				opts = append(opts, WithSessionStore(failingCompactionStore{Store: session.NewInMemorySessionStore(), err: errors.New("write failed")}))
			}
			rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), opts...)
			require.NoError(t, err)
			sink := &collectSink{}
			sess := twoMessageSession()
			rt.Summarize(t.Context(), sess, "", sink)
			usages := sink.budgetUsages()
			require.NotEmpty(t, usages)
			last := usages[len(usages)-1]
			assert.Equal(t, sess.ID, last.SessionID)
			require.Len(t, last.Budgets, 1)
			assert.Equal(t, int64(150), last.Budgets[0].Tokens)
			assert.Equal(t, tc.unpriced, last.Budgets[0].Unpriced)
			if !tc.unpriced {
				assert.InDelta(t, 0.002, last.Budgets[0].Cost, 1e-9)
			}
			var unpricedWarnings int
			for _, event := range sink.events {
				switch e := event.(type) {
				case *SessionCompactionEvent:
					if e.Status == "completed" {
						assert.Equal(t, tc.wantOutcome, e.Outcome)
					}
				case *WarningEvent:
					if tc.unpriced {
						assert.Contains(t, e.Message, "cannot price")
						unpricedWarnings++
					}
				}
			}
			if tc.unpriced {
				assert.Equal(t, 1, unpricedWarnings)
			}
		})
	}
}

func TestCompactionHookSummaryAllowedWithExhaustedBudget(t *testing.T) {
	t.Parallel()

	root := agent.New("root", "test", agent.WithModel(&mockProvider{id: "test/model"}),
		agent.WithHooks(&latest.HooksConfig{BeforeCompaction: []latest.HookDefinition{{Type: "builtin", Command: "free-summary"}}}))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)),
		WithBudget(&latest.BudgetConfig{MaxCost: 0.001}), WithModelStore(mockModelStoreWithLimit{limit: 100_000}))
	require.NoError(t, err)
	require.NoError(t, rt.hooksRegistry.RegisterBuiltin("free-summary", func(context.Context, *hooks.Input, []string) (*hooks.Output, error) {
		return &hooks.Output{HookSpecificOutput: &hooks.HookSpecificOutput{Summary: "free summary"}}, nil
	}))
	rt.ensureBudget()
	rt.recordBudget(session.New(), root, nil, new(0.001), 0, &collectSink{})
	sess := twoMessageSession()
	rt.Summarize(t.Context(), sess, "", &collectSink{})
	require.Len(t, sess.Messages, 3)
	assert.Equal(t, "free summary", sess.Messages[2].Summary)
	assert.InDelta(t, 0.001, rt.currentBudget().trackers[runBudgetName].snapshot().Cost, 1e-9)
}
