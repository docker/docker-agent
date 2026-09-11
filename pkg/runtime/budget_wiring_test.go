package runtime

import (
	"strings"
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

func (s *collectSink) warnings() []*WarningEvent {
	var out []*WarningEvent
	for _, e := range s.events {
		if w, ok := e.(*WarningEvent); ok {
			out = append(out, w)
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
	r.recordBudget(sess, a, &chat.Usage{InputTokens: 1000, OutputTokens: 200}, new(0.01), 30*time.Second, sink)

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
		r.recordBudget(sess, a, &chat.Usage{InputTokens: 100, OutputTokens: 100}, new(0.001), 10*time.Second, sink)
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
	r.recordBudget(sess, a, &chat.Usage{InputTokens: 500, OutputTokens: 500}, new(0.02), 10*time.Second, sink)

	r.ensureBudget()
	now = budgetEpoch.Add(20 * time.Second)
	r.recordBudget(sess, a, &chat.Usage{InputTokens: 500, OutputTokens: 500}, new(0.02), 10*time.Second, sink)

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

// TestEnforceBudgetEmitsCanonicalStopMessage pins the budget stop wiring:
// budget_exceeded embeds the assistant stop message under StopMessage,
// the session records the same canonical message right after, and the
// message_added event (whose payload is never serialized) follows the
// marker. JSON consumers only ever see the event's copy, so event and
// session content must be identical.
func TestEnforceBudgetEmitsCanonicalStopMessage(t *testing.T) {
	now := budgetEpoch
	r := budgetRuntime(t, func() time.Time { return now })
	r.ensureBudget()

	sess := session.New()
	a := agent.New("root", "test")
	sink := &collectSink{}
	r.recordBudget(sess, a, &chat.Usage{InputTokens: 100, OutputTokens: 100}, new(0.03), time.Second, sink)

	require.Equal(t, iterationStop, r.enforceBudget(t.Context(), sess, a, sink))

	var exceeded *BudgetExceededEvent
	var added *MessageAddedEvent
	exceededIdx, addedIdx := -1, -1
	for i, e := range sink.events {
		switch ev := e.(type) {
		case *BudgetExceededEvent:
			exceeded = ev
			exceededIdx = i
		case *MessageAddedEvent:
			added = ev
			addedIdx = i
		}
	}
	require.NotNil(t, exceeded, "enforceBudget must emit budget_exceeded")
	require.NotNil(t, added, "enforceBudget must emit message_added")
	assert.Less(t, exceededIdx, addedIdx, "the marker precedes the stop message")

	require.NotNil(t, exceeded.StopMessage)
	assert.Equal(t, "root", exceeded.StopMessage.AgentName)
	assert.Equal(t, chat.MessageRoleAssistant, exceeded.StopMessage.Message.Role)
	assert.Equal(t, exceeded.Message, exceeded.StopMessage.Message.Content)
	assert.Equal(t, budgetEpoch.Format(time.RFC3339), exceeded.StopMessage.Message.CreatedAt)

	messages := sess.GetAllMessages()
	require.Len(t, messages, 1)
	recorded := messages[0]
	assert.Equal(t, exceeded.StopMessage.AgentName, recorded.AgentName, "event and session must carry the same stop message")
	assert.Equal(t, exceeded.StopMessage.Message.Role, recorded.Message.Role)
	assert.Equal(t, exceeded.StopMessage.Message.Content, recorded.Message.Content)
	assert.Equal(t, exceeded.StopMessage.Message.CreatedAt, recorded.Message.CreatedAt)
	require.NotNil(t, added.Message)
	assert.Equal(t, recorded, *added.Message)
}

func TestEnforceBudgetWarnsOnceThenStillHardStops(t *testing.T) {
	now := budgetEpoch
	r := &LocalRuntime{now: func() time.Time { return now }}
	WithBudget(&latest.BudgetConfig{MaxCost: 0.50})(r)
	r.ensureBudget()

	sess := session.New()
	a := agent.New("root", "test")
	sink := &collectSink{}

	r.recordBudget(sess, a, &chat.Usage{InputTokens: 100, OutputTokens: 100}, new(0.40), time.Second, sink)
	require.Equal(t, iterationContinue, r.enforceBudget(t.Context(), sess, a, sink),
		"80% of max_cost must not stop the run")

	warns := sink.warnings()
	require.Len(t, warns, 1, "enforceBudget must emit exactly one Warning at 80%")
	assert.Contains(t, warns[0].Message, "used $0.40 of $0.50 budget.max_cost")
	assert.Contains(t, warns[0].Message, "Prefer cheaper tools")

	prompt := sess.GetMessages(a)
	var sawSystem bool
	for _, msg := range prompt {
		if msg.Role == chat.MessageRoleSystem && strings.Contains(msg.Content, "approaching the configured budget") {
			sawSystem = true
			assert.Equal(t, warns[0].Message, msg.Content, "the model-visible message must match the Warning event")
		}
	}
	assert.True(t, sawSystem, "the approaching warning must be in the next-turn prompt")

	eventCount := len(sink.events)
	require.Equal(t, iterationContinue, r.enforceBudget(t.Context(), sess, a, sink))
	assert.Len(t, sink.warnings(), 1, "a second enforceBudget must not re-warn")
	assert.Equal(t, eventCount, len(sink.events), "no extra events on the second approaching check")

	r.recordBudget(sess, a, &chat.Usage{InputTokens: 10, OutputTokens: 10}, new(0.15), time.Second, sink)
	require.Equal(t, iterationStop, r.enforceBudget(t.Context(), sess, a, sink),
		"crossing the ceiling after a warning must still hard-stop")

	var exceeded *BudgetExceededEvent
	for _, e := range sink.events {
		if ev, ok := e.(*BudgetExceededEvent); ok {
			exceeded = ev
		}
	}
	require.NotNil(t, exceeded, "hard-stop contract is unchanged")
	assert.Equal(t, "max_cost", exceeded.Limit)
	assert.Equal(t, "budget.max_cost", exceeded.ConfigPath)
	assert.Contains(t, exceeded.Message, "Execution stopped")
}
