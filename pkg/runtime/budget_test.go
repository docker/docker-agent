package runtime

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
)

var budgetEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestNewBudgetTrackerNilWhenNoLimitSet(t *testing.T) {
	tests := []struct {
		name string
		cfg  *latest.BudgetConfig
	}{
		{"nil config", nil},
		{"empty config", &latest.BudgetConfig{}},
		{"all zero", &latest.BudgetConfig{MaxCost: 0, MaxTokens: 0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Nil(t, newBudgetTracker(tt.cfg),
				"a config with no ceilings must leave the run unbudgeted")
		})
	}
}

func TestNilBudgetTrackerIsInert(t *testing.T) {
	var b *budgetTracker
	assert.NotPanics(t, func() {
		b.record("root", &chat.Usage{InputTokens: 10}, new(1.0), time.Second)
		assert.Nil(t, b.exceeded())
		assert.Equal(t, budgetSnapshot{}, b.snapshot())
		assert.False(t, b.unpricedSpend())
	})
}

func TestBudgetMaxCost(t *testing.T) {
	b := newBudgetTracker(&latest.BudgetConfig{MaxCost: 0.50})
	require.NotNil(t, b)

	b.record("root", &chat.Usage{InputTokens: 100}, new(0.20), time.Second)
	assert.Nil(t, b.exceeded(), "under budget must not trip")

	b.record("root", &chat.Usage{InputTokens: 100}, new(0.20), time.Second)
	assert.Nil(t, b.exceeded(), "$0.40 of $0.50 must not trip")

	b.record("root", &chat.Usage{InputTokens: 100}, new(0.15), time.Second)
	breach := b.exceeded()
	require.NotNil(t, breach, "$0.55 of $0.50 must trip")
	assert.Equal(t, budgetLimitCost, breach.Limit)
	assert.Equal(t, "$0.55", breach.Used)
	assert.Equal(t, "$0.50", breach.Max)
	assert.Contains(t, breach.Message(), "budget.max_cost")
}

func TestBudgetTripsOnExactLimit(t *testing.T) {
	b := newBudgetTracker(&latest.BudgetConfig{MaxCost: 0.50})
	b.record("root", &chat.Usage{}, new(0.50), time.Second)
	require.NotNil(t, b.exceeded())
}

func TestBudgetMaxTokens(t *testing.T) {
	b := newBudgetTracker(&latest.BudgetConfig{MaxTokens: 1000})

	b.record("root", &chat.Usage{InputTokens: 400, OutputTokens: 100}, nil, time.Second)
	assert.Nil(t, b.exceeded(), "500 of 1000 must not trip")

	b.record("root", &chat.Usage{InputTokens: 400, OutputTokens: 200}, nil, time.Second)
	breach := b.exceeded()
	require.NotNil(t, breach, "1100 of 1000 must trip")
	assert.Equal(t, budgetLimitTokens, breach.Limit)
	assert.Equal(t, "1100 tokens", breach.Used)
	assert.Equal(t, "1000 tokens", breach.Max)
}

func TestBudgetTokensAreMonotonicAndCountBothDirections(t *testing.T) {
	b := newBudgetTracker(&latest.BudgetConfig{MaxTokens: 100})
	for range 10 {
		b.record("root", &chat.Usage{InputTokens: 3, OutputTokens: 2}, nil, time.Second)
	}
	assert.Equal(t, int64(50), b.snapshot().Tokens,
		"10 turns x (3 in + 2 out) must accumulate to 50")
	assert.Nil(t, b.exceeded())

	for range 11 {
		b.record("root", &chat.Usage{InputTokens: 3, OutputTokens: 2}, nil, time.Second)
	}
	assert.Equal(t, int64(105), b.snapshot().Tokens)
	require.NotNil(t, b.exceeded())
}

func TestBudgetMaxTime(t *testing.T) {
	b := newBudgetTracker(&latest.BudgetConfig{MaxTime: latest.Duration{Duration: 10 * time.Minute}})

	b.record("root", &chat.Usage{}, nil, 9*time.Minute)
	assert.Nil(t, b.exceeded(), "9m of active work in a 10m budget must not trip")

	b.record("root", &chat.Usage{}, nil, 2*time.Minute)
	breach := b.exceeded()
	require.NotNil(t, breach, "11m of active work must trip a 10m budget")
	assert.Equal(t, budgetLimitTime, breach.Limit)
	assert.Equal(t, "11m0s", breach.Used)
	assert.Equal(t, "10m0s", breach.Max)
}

func TestBudgetMaxTimeIgnoresIdleTime(t *testing.T) {
	b := newBudgetTracker(&latest.BudgetConfig{MaxTime: latest.Duration{Duration: time.Minute}})
	b.record("root", &chat.Usage{}, nil, 2*time.Second)
	b.record("root", &chat.Usage{}, nil, 3*time.Second)
	assert.Equal(t, 5*time.Second, b.snapshot().Elapsed,
		"only active turn time counts, not wall-clock since the session began")
	assert.Nil(t, b.exceeded())
}

func TestBudgetUnsetLimitsNeverTrip(t *testing.T) {
	b := newBudgetTracker(&latest.BudgetConfig{MaxTime: latest.Duration{Duration: time.Hour}})
	b.record("root", &chat.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000}, new(999.0), time.Second)
	assert.Nil(t, b.exceeded(), "only max_time is set; cost and tokens must be ignored")
}

func TestBudgetUnpricedSpendIsFlaggedNotCountedAsFree(t *testing.T) {
	b := newBudgetTracker(&latest.BudgetConfig{MaxCost: 0.50})

	b.record("root", &chat.Usage{InputTokens: 5000, OutputTokens: 5000}, nil, time.Second)
	assert.True(t, b.unpricedSpend(), "usage with no price must flag the run as unpriced")
	assert.Zero(t, b.snapshot().Cost, "unpriceable spend must not invent a number")
	assert.Nil(t, b.exceeded(), "unpriced spend cannot trip a cost ceiling")
	assert.True(t, b.snapshot().Unpriced)
}

func TestBudgetPricedFreeCallIsNotUnpriced(t *testing.T) {
	b := newBudgetTracker(&latest.BudgetConfig{MaxCost: 0.50})
	b.record("root", &chat.Usage{InputTokens: 10}, new(0.0), time.Second)
	assert.False(t, b.unpricedSpend(), "a priced free call is not unpriced")
	assert.False(t, b.snapshot().Unpriced)
}

func TestBudgetUnpricedIrrelevantWithoutCostLimit(t *testing.T) {
	b := newBudgetTracker(&latest.BudgetConfig{MaxTokens: 1000})
	b.record("root", &chat.Usage{InputTokens: 10}, nil, time.Second)
	assert.False(t, b.unpricedSpend(), "no max_cost means unpriced spend is not a problem")
}

func TestBudgetReportsCostFirstWhenSeveralTrip(t *testing.T) {
	b := newBudgetTracker(&latest.BudgetConfig{
		MaxCost:   0.10,
		MaxTokens: 10,
		MaxTime:   latest.Duration{Duration: time.Minute},
	})
	b.record("root", &chat.Usage{InputTokens: 100, OutputTokens: 100}, new(5.0), time.Second)

	breach := b.exceeded()
	require.NotNil(t, breach)
	assert.Equal(t, budgetLimitCost, breach.Limit)
}

func TestBudgetSnapshotReportsLimitsAndTotals(t *testing.T) {
	b := newBudgetTracker(&latest.BudgetConfig{
		MaxCost:   2,
		MaxTokens: 1000,
		MaxTime:   latest.Duration{Duration: 10 * time.Minute},
	})
	b.record("root", &chat.Usage{InputTokens: 30, OutputTokens: 20}, new(0.25), time.Second)

	s := b.snapshot()
	assert.InDelta(t, 0.25, s.Cost, 1e-9)
	assert.InDelta(t, 2.0, s.MaxCost, 1e-9)
	assert.Equal(t, int64(50), s.Tokens)
	assert.Equal(t, int64(1000), s.MaxTokens)
	assert.Equal(t, time.Second, s.Elapsed, "Elapsed is accumulated active time")
	assert.Equal(t, 10*time.Minute, s.MaxTime)
}

func TestBudgetTrackerIsConcurrencySafe(t *testing.T) {
	b := newBudgetTracker(&latest.BudgetConfig{MaxCost: 1000})

	done := make(chan struct{})
	for range 8 {
		go func() {
			defer func() { done <- struct{}{} }()
			for range 50 {
				b.record("root", &chat.Usage{InputTokens: 1, OutputTokens: 1}, new(0.01), time.Second)
				b.exceeded()
				b.snapshot()
			}
		}()
	}
	for range 8 {
		<-done
	}

	s := b.snapshot()
	assert.Equal(t, int64(800), s.Tokens, "8 goroutines x 50 turns x 2 tokens")
	assert.InDelta(t, 4.0, s.Cost, 1e-6, "8 x 50 x $0.01")
}

func TestBudgetRecordToleratesNilUsage(t *testing.T) {
	b := newBudgetTracker(&latest.BudgetConfig{MaxTokens: 10})
	assert.NotPanics(t, func() { b.record("root", nil, nil, time.Second) })
	assert.Zero(t, b.snapshot().Tokens)
}

func TestBudgetPerAgentBreakdown(t *testing.T) {
	b := newBudgetTracker(&latest.BudgetConfig{MaxCost: 10})

	b.record("root", &chat.Usage{InputTokens: 100, OutputTokens: 100}, new(0.02), 2*time.Second)
	b.record("developer", &chat.Usage{InputTokens: 300, OutputTokens: 300}, new(0.09), 5*time.Second)
	b.record("root", &chat.Usage{InputTokens: 50, OutputTokens: 50}, new(0.01), time.Second)

	per := b.snapshot().PerAgent
	require.Len(t, per, 2)

	assert.Equal(t, "developer", per[0].AgentName)
	assert.InDelta(t, 0.09, per[0].Cost, 1e-9)
	assert.Equal(t, int64(600), per[0].Tokens)
	assert.Equal(t, 5*time.Second, per[0].Active)

	assert.Equal(t, "root", per[1].AgentName)
	assert.InDelta(t, 0.03, per[1].Cost, 1e-9)
	assert.Equal(t, int64(300), per[1].Tokens)
	assert.Equal(t, 3*time.Second, per[1].Active)

	s := b.snapshot()
	assert.InDelta(t, per[0].Cost+per[1].Cost, s.Cost, 1e-9)
	assert.Equal(t, per[0].Tokens+per[1].Tokens, s.Tokens)
}

func TestBudgetPerAgentEmptyBeforeSpend(t *testing.T) {
	b := newBudgetTracker(&latest.BudgetConfig{MaxCost: 1})
	assert.Empty(t, b.snapshot().PerAgent)
}

func budgetSetFixture() *budgetSet {
	return newBudgetSet(
		&latest.BudgetConfig{MaxCost: 1.00},
		map[string]latest.BudgetConfig{
			"tight":  {MaxCost: 0.10},
			"roomy":  {MaxTokens: 1_000_000},
			"unused": {MaxCost: 99},
		},
		map[string][]string{
			"root":      {"tight"},
			"developer": {"tight", "roomy"},
		},
	)
}

func TestBudgetSetNamedBudgetIsSharedNotCopied(t *testing.T) {
	s := budgetSetFixture()
	require.NotNil(t, s)

	for _, agent := range []string{"root", "developer"} {
		for _, nt := range s.budgetsFor(agent) {
			nt.Tracker.record(agent, &chat.Usage{OutputTokens: 10}, new(0.04), time.Second)
		}
	}

	tight := s.trackers["tight"]
	assert.InDelta(t, 0.08, tight.snapshot().Cost, 1e-9,
		"a named budget must be one shared pot across referencing agents")
	assert.Nil(t, s.exceededFor("root"), "$0.08 of $0.10 must not trip")

	for _, nt := range s.budgetsFor("root") {
		nt.Tracker.record("root", &chat.Usage{OutputTokens: 10}, new(0.04), time.Second)
	}
	for _, agent := range []string{"root", "developer"} {
		br := s.exceededFor(agent)
		require.NotNil(t, br, "shared pot exhausted must trip for %s", agent)
		assert.Equal(t, "tight", br.Budget)
		assert.Equal(t, budgetLimitCost, br.Limit)
	}
}

func TestBudgetSetChargesEveryReferencedBudget(t *testing.T) {
	s := budgetSetFixture()

	names := make([]string, 0)
	for _, nt := range s.budgetsFor("developer") {
		names = append(names, nt.Name)
	}
	assert.Equal(t, []string{runBudgetName, "tight", "roomy"}, names,
		"developer draws on the run-wide budget and both of its named budgets")

	names = names[:0]
	for _, nt := range s.budgetsFor("root") {
		names = append(names, nt.Name)
	}
	assert.Equal(t, []string{runBudgetName, "tight"}, names)
}

func TestBudgetSetUndeclaredAgentUsesRunBudget(t *testing.T) {
	s := budgetSetFixture()
	nts := s.budgetsFor("some-other-agent")
	require.Len(t, nts, 1)
	assert.Equal(t, runBudgetName, nts[0].Name)
}

func TestBudgetSetSkipsUnreferencedBudgets(t *testing.T) {
	s := budgetSetFixture()
	assert.NotContains(t, s.trackers, "unused")
	assert.NotContains(t, s.order, "unused")
}

func TestBudgetSetOrderIsStable(t *testing.T) {
	s := budgetSetFixture()
	assert.Equal(t, []string{runBudgetName, "roomy", "tight"}, s.order)
}

func TestBudgetSetRunWideBudgetTrips(t *testing.T) {
	s := newBudgetSet(
		&latest.BudgetConfig{MaxCost: 0.05},
		map[string]latest.BudgetConfig{"roomy": {MaxCost: 100}},
		map[string][]string{"root": {"roomy"}},
	)
	for _, nt := range s.budgetsFor("root") {
		nt.Tracker.record("root", &chat.Usage{OutputTokens: 10}, new(0.06), time.Second)
	}
	br := s.exceededFor("root")
	require.NotNil(t, br)
	assert.Equal(t, runBudgetName, br.Budget)
	assert.Contains(t, br.Message(), "budget.max_cost")
}

func TestBudgetBreachConfigPath(t *testing.T) {
	assert.Equal(t, "budget.max_cost",
		budgetBreach{Budget: runBudgetName, Limit: budgetLimitCost}.configPath())
	assert.Equal(t, "budgets.tight.max_tokens",
		budgetBreach{Budget: "tight", Limit: budgetLimitTokens}.configPath())
	assert.Equal(t, "budget.max_cost",
		budgetBreach{Limit: budgetLimitCost}.configPath())
}

func TestBudgetSetNilWhenNothingConfigured(t *testing.T) {
	assert.Nil(t, newBudgetSet(nil, nil, nil))
	assert.Nil(t, newBudgetSet(&latest.BudgetConfig{}, map[string]latest.BudgetConfig{}, map[string][]string{}))
	assert.Nil(t, newBudgetSet(nil, map[string]latest.BudgetConfig{"x": {MaxCost: 1}}, nil))
}

func TestBudgetSetSnapshotPerBudget(t *testing.T) {
	s := budgetSetFixture()
	for _, nt := range s.budgetsFor("developer") {
		nt.Tracker.record("developer", &chat.Usage{InputTokens: 5, OutputTokens: 5}, new(0.02), time.Second)
	}

	snaps := s.snapshot()
	require.Len(t, snaps, 3)
	assert.Equal(t, runBudgetName, snaps[0].Name)
	assert.Equal(t, "roomy", snaps[1].Name)
	assert.Equal(t, "tight", snaps[2].Name)

	assert.InDelta(t, 0.02, snaps[0].Snapshot.Cost, 1e-9)
	assert.InDelta(t, 1.00, snaps[0].Snapshot.MaxCost, 1e-9)
	assert.Equal(t, int64(10), snaps[1].Snapshot.Tokens)
	assert.InDelta(t, 0.10, snaps[2].Snapshot.MaxCost, 1e-9)
}

func TestBudgetConfigIsZero(t *testing.T) {
	assert.True(t, (*latest.BudgetConfig)(nil).IsZero())
	assert.True(t, (&latest.BudgetConfig{}).IsZero())
	assert.False(t, (&latest.BudgetConfig{MaxCost: 0.5}).IsZero())
	assert.False(t, (&latest.BudgetConfig{MaxTokens: 1}).IsZero())
	assert.False(t, (&latest.BudgetConfig{MaxTime: latest.Duration{Duration: time.Second}}).IsZero())
}
