package runtime

import (
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/evaluator"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/session"
)

func budgetExceededEvents(events []Event) []*BudgetExceededEvent {
	var out []*BudgetExceededEvent
	for _, ev := range events {
		if e, ok := ev.(*BudgetExceededEvent); ok {
			out = append(out, e)
		}
	}
	return out
}

func warnings(events []Event) []string {
	var out []string
	for _, ev := range events {
		if w, ok := ev.(*WarningEvent); ok {
			out = append(out, w.Message)
		}
	}
	return out
}

func assertNoAgentRan(t *testing.T, f *routingFixture) {
	t.Helper()
	for name, p := range f.providers {
		assert.Zero(t, p.calls.Load(), "%s must not run", name)
	}
	assert.Zero(t, f.router.calls.Load(), "the router must not run")
}

// A routing assessment is charged to the run budget. Once that crosses the
// limit the run stops; it never carries on with the default agent.
func TestRouting_BudgetExhaustedByAssessmentStopsInsteadOfDefaultRoute(t *testing.T) {
	t.Parallel()
	// choiceResult reports 12 tokens, so a 5 token budget is crossed by the assessment itself.
	f := newRoutingFixtureFor(t, latest.EventBeforeAgentRun,
		[]Opt{WithBudget(&latest.BudgetConfig{MaxTokens: 5})},
		scriptedAnswer{result: choiceResult("complex", defaultProbabilities("complex", 0.95))})

	sess := session.New(session.WithUserMessage("q"), session.WithNonInteractive(true))
	events := runRouted(t, f.rt, sess)

	assert.EqualValues(t, 1, f.eval.calls.Load(), "the assessment was attempted and charged")
	require.Len(t, budgetExceededEvents(events), 1)
	assert.Empty(t, routeEvents(events), "no route may be taken once the budget is exhausted")
	assertNoAgentRan(t, f)
	assert.Len(t, evaluationItems(sess), 1, "the attempt is still recorded")
	for _, d := range routingDecisions(sess) {
		assert.NotEqual(t, "clarifier", d.ToAgent, "the default agent must not be used")
	}
}

// A budget that is already spent blocks the assessment before it is sent.
func TestRouting_BudgetAlreadySpentBlocksAssessment(t *testing.T) {
	t.Parallel()
	f := newRoutingFixtureFor(t, latest.EventBeforeAgentRun,
		[]Opt{WithBudget(&latest.BudgetConfig{MaxTokens: 5})},
		scriptedAnswer{result: choiceResult("complex", defaultProbabilities("complex", 0.95))})
	f.rt.ensureBudget()
	f.rt.currentBudget().trackers[runBudgetName].record("root", &chat.Usage{InputTokens: 100}, nil, 0)

	sess := session.New(session.WithUserMessage("q"), session.WithNonInteractive(true))
	events := runRouted(t, f.rt, sess)

	assert.Zero(t, f.eval.calls.Load(), "no assessment may be sent once the budget is spent")
	assert.Empty(t, routeEvents(events))
	assertNoAgentRan(t, f)
}

// The same guarantee holds for completion routing.
func TestRouting_BudgetExhaustedAfterCompletionDoesNotContinue(t *testing.T) {
	t.Parallel()
	f := newRoutingFixtureFor(t, latest.EventAfterAgentComplete,
		[]Opt{WithBudget(&latest.BudgetConfig{MaxTokens: 5})},
		scriptedAnswer{result: choiceResult("complex", defaultProbabilities("complex", 0.95))})

	sess := session.New(session.WithUserMessage("q"), session.WithNonInteractive(true))
	events := runRouted(t, f.rt, sess)

	assert.Empty(t, routeEvents(events), "a spent budget must stop the run, not continue it")
	assert.Zero(t, f.providers["specialist"].calls.Load())
	assert.Zero(t, f.providers["clarifier"].calls.Load())
}

// Broken accounting from an evaluator is terminal: the run stops and the
// default agent is never used as a substitute.
func TestRouting_InvalidAccountingNeverFallsBackToDefaultAgent(t *testing.T) {
	t.Parallel()
	negativeCost, nan, inf := -1.0, math.NaN(), math.Inf(1)
	tests := []struct {
		name   string
		result func() *evaluator.Result
	}{
		{"negative cost", func() *evaluator.Result {
			r := choiceResult("complex", defaultProbabilities("complex", 0.95))
			r.Cost = &negativeCost
			return r
		}},
		{"NaN cost", func() *evaluator.Result {
			r := choiceResult("complex", defaultProbabilities("complex", 0.95))
			r.Cost = &nan
			return r
		}},
		{"infinite cost", func() *evaluator.Result {
			r := choiceResult("complex", defaultProbabilities("complex", 0.95))
			r.Cost = &inf
			return r
		}},
		{"negative tokens", func() *evaluator.Result {
			r := choiceResult("complex", defaultProbabilities("complex", 0.95))
			r.Usage = evaluator.Usage{InputTokens: -5, OutputTokens: 2}
			return r
		}},
		{"token overflow", func() *evaluator.Result {
			r := choiceResult("complex", defaultProbabilities("complex", 0.95))
			r.Usage = evaluator.Usage{InputTokens: math.MaxInt64, OutputTokens: 1}
			return r
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newEvaluatorRoutingFixture(t, scriptedAnswer{result: tt.result()})

			sess := session.New(session.WithUserMessage("q"), session.WithNonInteractive(true))
			events := runRouted(t, f.rt, sess)

			require.Len(t, routedErrors(events), 1, "invalid accounting must stop the run with an error")
			assert.Empty(t, routeEvents(events))
			assertNoAgentRan(t, f)
			records := evaluationItems(sess)
			require.Len(t, records, 1, "the attempt is recorded once")
			assert.Nil(t, records[0].Cost, "an invalid price is stored as unknown, never as a number")
		})
	}
}

// An evaluator that reports no usage is not an error: the route is taken, the
// spend is recorded as unknown, and the user is warned that cost is incomplete.
func TestRouting_UnknownUsageStillRoutesAndIsFlagged(t *testing.T) {
	t.Parallel()
	result := choiceResult("complex", defaultProbabilities("complex", 0.95))
	result.Usage, result.Cost = evaluator.Usage{}, nil
	f := newEvaluatorRoutingFixture(t, scriptedAnswer{result: result})

	sess := session.New(session.WithUserMessage("q"), session.WithNonInteractive(true))
	events := runRouted(t, f.rt, sess)

	assert.Empty(t, routedErrors(events))
	routes := routeEvents(events)
	require.Len(t, routes, 1)
	assert.Equal(t, "specialist", routes[0].ToAgent)
	records := evaluationItems(sess)
	require.Len(t, records, 1)
	assert.Nil(t, records[0].Cost)
	assert.Nil(t, records[0].Usage, "missing usage stays unknown instead of becoming zero")
	var flagged bool
	for _, w := range warnings(events) {
		flagged = flagged || strings.Contains(w, "spend is unknown")
	}
	assert.True(t, flagged, "unknown spend must produce a warning")
}

// With tool-mode structured output the answer arrives as a tool call. Once it
// is accepted it is a completion like any other, and the hook sees the
// validated JSON.
func TestRouting_AcceptedStructuredOutputRoutesOnward(t *testing.T) {
	t.Parallel()
	st := newSelectorTeam(t, map[string]hooks.BuiltinFunc{"review": routeTo("reviewer")},
		func(name string, cfg *latest.HooksConfig, opts *[]agent.Opt) {
			if name == "worker" {
				cfg.AfterAgentComplete = selectorHook("review")
				*opts = append(*opts, agent.WithStructuredOutput(toolModeStructuredOutput()),
					agent.WithRouting(agent.Routing{AllowedAgents: []string{"reviewer"}}))
			}
		})
	// Script the worker's only model: agent.WithModel appends to a pool, so adding
	// another provider would let the runtime pick either one at random.
	st.providers["worker"].stream = func() chat.MessageStream { return outputCallStream("call_1", `{"answer":"hi"}`) }
	require.NoError(t, st.rt.SetCurrentAgent(t.Context(), "worker"))

	sess := session.New(session.WithUserMessage("answer me"), session.WithNonInteractive(true))
	events := runRouted(t, st.rt, sess)

	assert.Empty(t, routedErrors(events))
	assert.EqualValues(t, 1, st.providers["worker"].calls.Load(), "the worker answers once; an accepted answer is terminal")
	assert.EqualValues(t, 1, st.providers["reviewer"].calls.Load(), "the accepted answer must continue to the reviewer")
	assert.Equal(t, "reviewer answer", sess.GetLastAssistantMessageContent())

	var completion *hooks.Input
	for _, in := range st.recorded() {
		if in.HookEventName == hooks.EventAfterAgentComplete {
			completion = &in
			break
		}
	}
	require.NotNil(t, completion, "after_agent_complete must fire for an accepted structured answer")
	assert.JSONEq(t, `{"answer":"hi"}`, completion.Output)

	decisions := routingDecisions(sess)
	require.Len(t, decisions, 1)
	assert.Equal(t, "reviewer", decisions[0].ToAgent)
}

// A plain-text stop is not an accepted structured answer, so it must not
// route: the run ends with the structured output error instead.
func TestRouting_RejectedStructuredOutputDoesNotRoute(t *testing.T) {
	t.Parallel()
	st := newSelectorTeam(t, map[string]hooks.BuiltinFunc{"review": routeTo("reviewer")},
		func(name string, cfg *latest.HooksConfig, opts *[]agent.Opt) {
			if name == "worker" {
				cfg.AfterAgentComplete = selectorHook("review")
				*opts = append(*opts, agent.WithStructuredOutput(toolModeStructuredOutput()),
					agent.WithRouting(agent.Routing{AllowedAgents: []string{"reviewer"}}))
			}
		})
	require.NoError(t, st.rt.SetCurrentAgent(t.Context(), "worker"))

	sess := session.New(session.WithUserMessage("answer me"), session.WithNonInteractive(true))
	events := runRouted(t, st.rt, sess)

	var sawStructuredError bool
	for _, e := range routedErrors(events) {
		sawStructuredError = sawStructuredError || e.Code == ErrorCodeStructuredOutputFailed
	}
	assert.True(t, sawStructuredError)
	assert.Zero(t, st.providers["reviewer"].calls.Load(), "an unaccepted answer must never continue to another agent")
	for _, in := range st.recorded() {
		assert.NotEqual(t, hooks.EventAfterAgentComplete, in.HookEventName)
	}
	assert.Empty(t, routingDecisions(sess))
}
