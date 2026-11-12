package runtime

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
)

func routingDecisions(sess *session.Session) []*session.RoutingDecision {
	return sess.RoutingDecisionHistory()
}

func routingDecisionEvents(events []Event) []*RoutingDecisionEvent {
	var out []*RoutingDecisionEvent
	for _, ev := range events {
		if e, ok := ev.(*RoutingDecisionEvent); ok {
			out = append(out, e)
		}
	}
	return out
}

func TestRoutingDecision_EvaluatorRouteIsRecorded(t *testing.T) {
	t.Parallel()
	f := newEvaluatorRoutingFixture(t, scriptedAnswer{result: choiceResult("complex", defaultProbabilities("complex", 0.9))})

	sess := session.New(session.WithUserMessage("Diagnose this deadlock"), session.WithNonInteractive(true))
	events := runRouted(t, f.rt, sess)

	decisions := routingDecisions(sess)
	require.Len(t, decisions, 1)
	d := decisions[0]
	assert.NotEmpty(t, d.ID)
	assert.NotEmpty(t, d.InvocationID)
	assert.Equal(t, "step-1", d.StepID)
	assert.Equal(t, "before_agent_run", d.Phase)
	assert.Equal(t, "root", d.FromAgent)
	assert.Equal(t, "specialist", d.ToAgent)
	assert.Equal(t, hooks.TransitionActionRoute, d.Action)
	assert.Equal(t, "task_route", d.Evaluator)
	assert.Equal(t, "complex", d.Selected)
	assert.Equal(t, "judge-1", d.Model)
	require.NotNil(t, d.Probability)
	assert.InDelta(t, 0.9, *d.Probability, 1e-9)
	assert.Empty(t, d.FallbackReason)

	emitted := routingDecisionEvents(events)
	require.Len(t, emitted, 1)
	assert.Equal(t, d, emitted[0].Decision)
	assert.Equal(t, sess.ID, emitted[0].SessionID)
}

func TestRoutingDecision_FallbackReasonIsRecorded(t *testing.T) {
	t.Parallel()
	f := newEvaluatorRoutingFixture(t, scriptedAnswer{result: choiceResult("complex", defaultProbabilities("complex", 0.5))})

	sess := session.New(session.WithUserMessage("hmm"), session.WithNonInteractive(true))
	runRouted(t, f.rt, sess)

	decisions := routingDecisions(sess)
	require.Len(t, decisions, 1)
	assert.Equal(t, "clarifier", decisions[0].ToAgent)
	assert.Equal(t, hooks.FallbackBelowThreshold, decisions[0].FallbackReason)
	assert.Equal(t, "complex", decisions[0].Selected)
}

func TestRoutingDecision_NoRouteAndBlockedAreRecorded(t *testing.T) {
	t.Parallel()
	blocked := func(context.Context, *hooks.Input, []string) (*hooks.Output, error) {
		return &hooks.Output{Decision: hooks.DecisionBlockValue, Reason: "policy says no"}, nil
	}
	tests := []struct {
		name       string
		selector   hooks.BuiltinFunc
		wantAction string
		wantReason string
	}{
		{"no route", noRoute, routingActionNone, ""},
		{"blocked", blocked, routingActionBlocked, "policy says no"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := newSelectorTeam(t, map[string]hooks.BuiltinFunc{"select": tt.selector},
				func(name string, cfg *latest.HooksConfig, opts *[]agent.Opt) {
					if name == "root" {
						cfg.BeforeAgentRun = selectorHook("select")
						*opts = append(*opts, agent.WithRouting(agent.Routing{AllowedAgents: []string{"worker"}}))
					}
				})

			sess := session.New(session.WithUserMessage("go"), session.WithNonInteractive(true))
			runRouted(t, st.rt, sess)

			decisions := routingDecisions(sess)
			require.Len(t, decisions, 1)
			assert.Equal(t, tt.wantAction, decisions[0].Action)
			assert.Equal(t, tt.wantReason, decisions[0].Reason)
			assert.Equal(t, "root", decisions[0].FromAgent)
			assert.Empty(t, decisions[0].ToAgent)
		})
	}
}

func TestRoutingDecision_ForcedHandoffIsRecorded(t *testing.T) {
	t.Parallel()
	st := newSelectorTeam(t, map[string]hooks.BuiltinFunc{"enter": noRoute},
		func(name string, cfg *latest.HooksConfig, opts *[]agent.Opt) {
			if name == "root" {
				cfg.BeforeAgentRun = selectorHook("enter")
				*opts = append(*opts, agent.WithRouting(agent.Routing{AllowedAgents: []string{"worker"}}))
			}
		})
	root, err := st.rt.team.Agent("root")
	require.NoError(t, err)
	worker, err := st.rt.team.Agent("worker")
	require.NoError(t, err)
	agent.WithForceHandoff(worker)(root)

	sess := session.New(session.WithUserMessage("go"), session.WithNonInteractive(true))
	runRouted(t, st.rt, sess)

	decisions := routingDecisions(sess)
	require.Len(t, decisions, 2)
	assert.Equal(t, routingActionNone, decisions[0].Action)
	assert.Equal(t, agentSwitchKindForceHandoff, decisions[1].Action)
	assert.Equal(t, "worker", decisions[1].ToAgent)
	assert.Equal(t, agentSwitchKindForceHandoff, decisions[1].Phase)
}

func TestRoutingDecision_PersistsAndReloadsWithoutTaskText(t *testing.T) {
	t.Parallel()
	store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	f := newEvaluatorRoutingFixture(t, scriptedAnswer{result: choiceResult("complex", defaultProbabilities("complex", 0.9))})
	sess := session.New(session.WithUserMessage("SECRET-TASK-TEXT"), session.WithNonInteractive(true))
	observer := newPersistenceObserver(store)
	observer.OnRunStart(t.Context(), sess)
	for _, ev := range runRouted(t, f.rt, sess) {
		observer.OnEvent(t.Context(), sess, ev)
	}

	reloaded, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	want, got := routingDecisions(sess), routingDecisions(reloaded)
	require.Len(t, got, 1)
	assert.True(t, want[0].CreatedAt.Equal(got[0].CreatedAt), "stored time must round-trip")
	want[0].CreatedAt, got[0].CreatedAt = time.Time{}, time.Time{}
	assert.Equal(t, want, got)

	// A replayed event must not duplicate the stored decision.
	observer.OnEvent(t.Context(), sess, &RoutingDecisionEvent{SessionID: sess.ID, Decision: routingDecisions(sess)[0]})
	reloaded, err = store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	assert.Len(t, routingDecisions(reloaded), 1)

	payload, err := json.Marshal(routingDecisions(reloaded))
	require.NoError(t, err)
	assert.NotContains(t, string(payload), "SECRET-TASK-TEXT")
}
