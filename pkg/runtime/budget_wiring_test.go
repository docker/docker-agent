package runtime

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/session"
)

type collectSink struct{ events []Event }

func (s *collectSink) Emit(e Event) { s.events = append(s.events, e) }

func (s *collectSink) budgetUsages() []*BudgetUsageEvent {
	var out []*BudgetUsageEvent
	for _, e := range s.events {
		if b, ok := e.(*BudgetUsageEvent); ok {
			out = append(out, b)
		}
	}
	return out
}

func budgetRuntime(t *testing.T, clock func() time.Time) *LocalRuntime {
	t.Helper()
	r := &LocalRuntime{now: clock}
	WithBudget(&latest.BudgetConfig{MaxCost: 0.05, MaxTokens: 15000, MaxTime: latest.Duration{Duration: 2 * time.Minute}})(r)
	WithNamedBudgets(
		map[string]latest.BudgetConfig{
			"shell-work": {MaxCost: 0.03, MaxTokens: 8000},
		},
		map[string][]string{"root": {"shell-work"}},
	)(r)
	return r
}

func TestRecordBudgetEmitsNonZeroReading(t *testing.T) {
	now := budgetEpoch
	r := budgetRuntime(t, func() time.Time { return now })
	r.ensureBudget()

	sess := session.New()
	a := agent.New("root", "test")
	sink := &collectSink{}

	now = budgetEpoch.Add(30 * time.Second)
	r.recordBudget(sess, a, &chat.Usage{InputTokens: 1000, OutputTokens: 200}, ptr(0.01), 30*time.Second, sink)

	usages := sink.budgetUsages()
	require.Len(t, usages, 1, "recordBudget must emit exactly one budget_usage event")

	byName := map[string]BudgetStatus{}
	for _, b := range usages[0].Budgets {
		byName[b.Name] = b
	}
	require.Contains(t, byName, runBudgetName)
	require.Contains(t, byName, "shell-work")

	run := byName[runBudgetName]
	assert.InDelta(t, 0.01, run.Cost, 1e-9, "run budget must see the turn's cost")
	assert.Equal(t, int64(1200), run.Tokens, "run budget must see the turn's tokens")
	assert.InDelta(t, 30, run.ElapsedSeconds, 0.001, "elapsed must advance with the clock")

	sw := byName["shell-work"]
	assert.InDelta(t, 0.01, sw.Cost, 1e-9)
	assert.Equal(t, int64(1200), sw.Tokens)

	require.Len(t, run.PerAgent, 1)
	assert.Equal(t, "root", run.PerAgent[0].AgentName)
	assert.Equal(t, int64(1200), run.PerAgent[0].Tokens)
}

func TestRecordBudgetAccumulatesAcrossTurns(t *testing.T) {
	now := budgetEpoch
	r := budgetRuntime(t, func() time.Time { return now })
	r.ensureBudget()

	sess := session.New()
	a := agent.New("root", "test")
	sink := &collectSink{}

	for i := range 3 {
		now = budgetEpoch.Add(time.Duration(i+1) * 10 * time.Second)
		r.recordBudget(sess, a, &chat.Usage{InputTokens: 100, OutputTokens: 100}, ptr(0.001), 10*time.Second, sink)
	}

	usages := sink.budgetUsages()
	require.Len(t, usages, 3)
	last := usages[2]
	for _, b := range last.Budgets {
		if b.Name != runBudgetName {
			continue
		}
		assert.Equal(t, int64(600), b.Tokens, "3 turns x 200 tokens must accumulate")
		assert.InDelta(t, 0.003, b.Cost, 1e-9, "3 turns x $0.001 must accumulate")
		assert.InDelta(t, 30, b.ElapsedSeconds, 0.001)
	}
}

func TestRecordBudgetNilUsageIsNotSilentZero(t *testing.T) {
	now := budgetEpoch
	r := budgetRuntime(t, func() time.Time { return now })
	r.ensureBudget()

	sink := &collectSink{}
	r.recordBudget(session.New(), agent.New("root", "test"), nil, nil, time.Second, sink)

	usages := sink.budgetUsages()
	require.Len(t, usages, 1)
	for _, b := range usages[0].Budgets {
		assert.Zero(t, b.Tokens)
	}
}

func TestBudgetSurvivesAcrossMessages(t *testing.T) {
	now := budgetEpoch
	r := budgetRuntime(t, func() time.Time { return now })

	sess := session.New()
	a := agent.New("root", "test")
	sink := &collectSink{}

	r.ensureBudget()
	now = budgetEpoch.Add(10 * time.Second)
	r.recordBudget(sess, a, &chat.Usage{InputTokens: 500, OutputTokens: 500}, ptr(0.02), 10*time.Second, sink)

	r.ensureBudget()
	now = budgetEpoch.Add(20 * time.Second)
	r.recordBudget(sess, a, &chat.Usage{InputTokens: 500, OutputTokens: 500}, ptr(0.02), 10*time.Second, sink)

	usages := sink.budgetUsages()
	last := usages[len(usages)-1]
	for _, b := range last.Budgets {
		if b.Name != runBudgetName {
			continue
		}
		assert.Equal(t, int64(2000), b.Tokens,
			"spend from earlier messages in the same session must still count")
		assert.InDelta(t, 0.04, b.Cost, 1e-9,
			"a budget that resets per message lets a session spend the ceiling on every turn")
	}
}
