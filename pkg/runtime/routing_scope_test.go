package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/evaluator"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

// scopedRoutingRuntime builds a routed root whose evaluator scope is rootScope
// (nil leaves the agent unbound) next to a different team-level evaluator.
func scopedRoutingRuntime(t *testing.T, rootScope map[string]evaluator.Evaluator, teamEval evaluator.Evaluator) (*LocalRuntime, map[string]*scriptedProvider) {
	t.Helper()
	providers := map[string]*scriptedProvider{}
	var agents []*agent.Agent
	for _, name := range []string{"quick", "specialist", "clarifier"} {
		providers[name] = &scriptedProvider{reply: name + " answer"}
		agents = append(agents, agent.New(name, name+" instructions", agent.WithModel(providers[name])))
	}
	providers["root"] = &scriptedProvider{reply: "router must not answer"}
	opts := []agent.Opt{
		agent.WithModel(providers["root"]),
		agent.WithRouting(agent.Routing{AllowedAgents: []string{"quick", "specialist", "clarifier"}, DefaultAgent: "clarifier"}),
		agent.WithHooks(evaluatorRoutingHooks(latest.EventBeforeAgentRun, 0.85)),
	}
	if rootScope != nil {
		opts = append(opts, agent.WithEvaluators(rootScope))
	}
	root := agent.New("root", "root instructions", opts...)
	tm := team.New(team.WithAgents(append([]*agent.Agent{root}, agents...)...),
		team.WithEvaluators(map[string]evaluator.Evaluator{"task_route": teamEval}))
	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, rt.Close()) })
	return rt, providers
}

// An agent loaded from its own configuration must use the evaluator it
// declared, even when the team defines another one under the same name.
func TestRouting_AgentEvaluatorScopeWinsOverTeamEvaluator(t *testing.T) {
	t.Parallel()
	own := &scriptedEvaluator{answers: []scriptedAnswer{{result: choiceResult("complex", defaultProbabilities("complex", 0.95))}}}
	teamEval := &scriptedEvaluator{answers: []scriptedAnswer{{result: choiceResult("simple", defaultProbabilities("simple", 0.95))}}}
	rt, providers := scopedRoutingRuntime(t, map[string]evaluator.Evaluator{"task_route": own}, teamEval)

	sess := session.New(session.WithUserMessage("q"), session.WithNonInteractive(true))
	events := runRouted(t, rt, sess)

	assert.Empty(t, routedErrors(events))
	assert.EqualValues(t, 1, own.calls.Load(), "the agent's own evaluator decides")
	assert.Zero(t, teamEval.calls.Load(), "the team-level evaluator must not be consulted")
	assert.EqualValues(t, 1, providers["specialist"].calls.Load())
	assert.Zero(t, providers["quick"].calls.Load())
}

// A bound scope that lacks the named evaluator fails closed. It never borrows
// the team's evaluator and never falls back to the default agent.
func TestRouting_BoundScopeNeverBorrowsTeamEvaluator(t *testing.T) {
	t.Parallel()
	teamEval := &scriptedEvaluator{answers: []scriptedAnswer{{result: choiceResult("complex", defaultProbabilities("complex", 0.95))}}}
	rt, providers := scopedRoutingRuntime(t, map[string]evaluator.Evaluator{}, teamEval)

	sess := session.New(session.WithUserMessage("q"), session.WithNonInteractive(true))
	events := runRouted(t, rt, sess)

	require.Len(t, routedErrors(events), 1, "a missing evaluator must stop the run")
	assert.Zero(t, teamEval.calls.Load(), "the team-level evaluator must not be borrowed")
	assert.Empty(t, routeEvents(events))
	for name, p := range providers {
		assert.Zero(t, p.calls.Load(), "%s must not run", name)
	}
}

// A programmatic agent with no bound scope still uses the team's evaluators.
func TestRouting_UnboundAgentUsesTeamEvaluator(t *testing.T) {
	t.Parallel()
	teamEval := &scriptedEvaluator{answers: []scriptedAnswer{{result: choiceResult("complex", defaultProbabilities("complex", 0.95))}}}
	rt, providers := scopedRoutingRuntime(t, nil, teamEval)

	sess := session.New(session.WithUserMessage("q"), session.WithNonInteractive(true))
	events := runRouted(t, rt, sess)

	assert.Empty(t, routedErrors(events))
	assert.EqualValues(t, 1, teamEval.calls.Load())
	assert.EqualValues(t, 1, providers["specialist"].calls.Load())
}
