package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

func TestSessionStateRestoredEvaluatorCosts(t *testing.T) {
	t.Parallel()

	root, child, nested := session.New(), session.New(), session.New()
	root.Messages = []session.Item{
		restoredCostMessage("root", 0.5),
		{Summary: "compacted", Cost: 0.125},
	}
	child.Messages = []session.Item{restoredCostMessage("worker", 0.125)}
	rootEvaluation := &session.Evaluation{ID: "root-evaluation", AgentName: "root", Cost: new(0.25)}
	root.AddEvaluation(rootEvaluation)
	root.AddEvaluationUsageRecord(rootEvaluation)
	child.AddEvaluation(&session.Evaluation{ID: "child-evaluation", AgentName: "worker", Cost: new(0.0625)})
	nested.AddEvaluation(&session.Evaluation{ID: "nested-evaluation", AgentName: "worker", Cost: new(0.0625)})
	child.AddSubSession(nested)
	root.AddSubSession(child)
	root.AddEvaluation(&session.Evaluation{ID: "free", AgentName: "free", Cost: new(float64)})
	root.AddEvaluation(&session.Evaluation{ID: "unknown", AgentName: "unknown"})
	root.AddEvaluation(&session.Evaluation{ID: "unattributed", Cost: new(0.125)})
	root.AddEvaluationUsageRecord(&session.Evaluation{ID: "transient-only", AgentName: "transient", Cost: new(99.0)})

	state := NewSessionState(root)
	for range 2 {
		state.SeedRestoredCosts(root)
		for name, want := range map[string]float64{"root": 0.75, "worker": 0.25, "free": 0} {
			cost, ok := state.AgentCost(name)
			require.True(t, ok, name)
			assert.InDelta(t, want, cost, 1e-12, name)
			_, hasUsage := state.AgentUsage(name)
			assert.False(t, hasUsage, "evaluator tokens must not fabricate chat context")
		}
		for _, name := range []string{"unknown", "transient", ""} {
			_, ok := state.AgentCost(name)
			assert.False(t, ok, name)
		}
	}

	// Restored child costs are already included in the root's snapshot baseline.
	for range 2 {
		state.SetAgentUsage(root.ID, "root", *runtime.SessionUsage(root, 10000))
	}
	cost, ok := state.AgentCost("root")
	require.True(t, ok)
	assert.InDelta(t, 0.75, cost, 1e-12)

	root.AddEvaluation(&session.Evaluation{ID: "new-root", AgentName: "root", Cost: new(0.25)})
	live := session.New()
	live.AddEvaluation(&session.Evaluation{ID: "new-child", AgentName: "worker", Cost: new(0.125)})
	root.AddLiveSubSession(live)
	for range 2 {
		state.SetAgentUsage(root.ID, "root", *runtime.SessionUsage(root, 10000))
		state.SetAgentUsage(live.ID, "worker", *runtime.SessionUsage(live, 10000))
	}
	cost, ok = state.AgentCost("root")
	require.True(t, ok)
	assert.InDelta(t, 1.0, cost, 1e-12)
	cost, ok = state.AgentCost("worker")
	require.True(t, ok)
	assert.InDelta(t, 0.375, cost, 1e-12, "live child cost must not also enter the root snapshot")
}
