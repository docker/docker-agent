package runtime

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
)

func TestNativeCompactionSharesBudgets(t *testing.T) {
	t.Parallel()

	var elapsed atomic.Int64
	comp := &mockCompactor{
		id: "anthropic/claude-test", opts: nativeOpts, result: nativeResult(),
		onCall: func() { elapsed.Add(int64(3 * time.Second)) },
	}
	worker := agent.New("worker", "test", agent.WithModel(comp))
	rt := newNativeRuntime(t, worker,
		WithClock(func() time.Time { return budgetEpoch.Add(time.Duration(elapsed.Load())) }),
		WithBudget(&latest.BudgetConfig{MaxCost: 1}),
		WithNamedBudgets(map[string]latest.BudgetConfig{"work": {MaxTokens: 1100}, "other": {MaxCost: 1}},
			map[string][]string{"worker": {"work"}, "root": {"other"}}))
	sess := nativeTestSession()
	sess.AgentName = "worker"
	sess.ParentID = session.New().ID
	sink := &collectSink{}
	rt.Summarize(t.Context(), sess, "", sink)

	require.Equal(t, 1, comp.callCount())
	require.NotNil(t, rt.currentBudget(), "manual compaction initializes the budget")
	assert.InDelta(t, 0.002, sess.TotalCost(), 1e-9)
	for _, name := range []string{runBudgetName, "work"} {
		snapshot := rt.currentBudget().trackers[name].snapshot()
		assert.InDelta(t, 0.002, snapshot.Cost, 1e-9)
		assert.Equal(t, int64(1100), snapshot.Tokens)
		assert.Equal(t, 3*time.Second, snapshot.Elapsed)
		require.Len(t, snapshot.PerAgent, 1)
		assert.Equal(t, "worker", snapshot.PerAgent[0].AgentName)
	}
	assert.Zero(t, rt.currentBudget().trackers["other"].snapshot().Cost)
	usages := sink.budgetUsages()
	require.NotEmpty(t, usages)
	assert.Equal(t, sess.ID, usages[len(usages)-1].SessionID)

	before := sess.MessagesSnapshot()
	sink = &collectSink{}
	rt.Summarize(t.Context(), sess, "", sink)
	assert.Equal(t, 1, comp.callCount(), "exhausted budgets prevent further native calls")
	assert.Equal(t, before, sess.MessagesSnapshot())
	var skipped, warned bool
	for _, event := range sink.events {
		switch e := event.(type) {
		case *SessionCompactionEvent:
			if e.Status == "completed" {
				assert.Equal(t, CompactionOutcomeSkipped, e.Outcome)
				skipped = true
			}
		case *WarningEvent:
			assert.Contains(t, e.Message, "budgets.work.max_tokens")
			warned = true
		case *ErrorEvent, *SessionSummaryEvent, *BudgetExceededEvent:
			t.Errorf("unexpected event: %T", event)
		}
	}
	assert.True(t, skipped)
	assert.True(t, warned)
}

func TestNativeCompactionBudgetRecordsUnappliedSummaries(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		empty       bool
		storeError  bool
		unpriced    bool
		wantOutcome string
	}{
		{name: "empty summary", empty: true, wantOutcome: CompactionOutcomeSkipped},
		{name: "persistence failure", storeError: true, wantOutcome: CompactionOutcomeFailed},
		{name: "unpriced summary", unpriced: true, wantOutcome: CompactionOutcomeApplied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			result := nativeResult()
			if tc.empty {
				result.Summary = " "
			}
			comp := &mockCompactor{id: "anthropic/claude-test", opts: nativeOpts, result: result}
			root := agent.New("root", "test", agent.WithModel(comp))
			opts := []Opt{WithBudget(&latest.BudgetConfig{MaxCost: 1})}
			if tc.unpriced {
				opts = append(opts, WithModelStore(modelsdev.NewDatabaseStore(&modelsdev.Database{})))
			}
			if tc.storeError {
				opts = append(opts, WithSessionStore(failingCompactionStore{Store: session.NewInMemorySessionStore(), err: errors.New("write failed")}))
			}
			rt := newNativeRuntime(t, root, opts...)
			sess := nativeTestSession()
			sink := &collectSink{}
			rt.Summarize(t.Context(), sess, "", sink)
			usages := sink.budgetUsages()
			require.NotEmpty(t, usages)
			last := usages[len(usages)-1]
			assert.Equal(t, sess.ID, last.SessionID)
			require.Len(t, last.Budgets, 1)
			assert.Equal(t, int64(1100), last.Budgets[0].Tokens)
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
