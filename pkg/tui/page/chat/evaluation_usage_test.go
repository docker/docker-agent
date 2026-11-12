package chat

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/app"
	chattypes "github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestEvaluationUsageEventKeepsTransientAccountingSeparate(t *testing.T) {
	t.Parallel()

	for _, persisted := range []bool{false, true} {
		name := "remote history"
		if persisted {
			name = "local persisted items"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			root, child := session.New(), session.New()
			root.SetUsage(100, 20)
			child.SetUsage(40, 5)
			root.AddMessage(session.UserMessage("Run the tool."))
			rootEvaluation := &session.Evaluation{
				ID: "root-request", Evaluator: "safety", AgentName: "root", Model: "judge-model",
				Usage: &chattypes.Usage{InputTokens: 5000, OutputTokens: 100}, Cost: new(0.25),
			}
			childEvaluation := &session.Evaluation{
				ID: "child-request", Evaluator: "safety", AgentName: "worker", Model: "judge-model",
				Usage: &chattypes.Usage{InputTokens: 2000, OutputTokens: 50}, Cost: new(0.125),
			}
			if persisted {
				root.AddEvaluation(rootEvaluation)
				child.AddEvaluation(childEvaluation)
				root.AddLiveSubSession(child)
			}
			state := service.NewSessionState(root)
			state.SetCurrentAgentName("root")
			p := New(animation.NewRuntime(), t.Context(), app.New(t.Context(), queueTestRuntime{}, root), state).(*chatPage)
			before := root.MessagesSnapshot()
			beforeCost := root.TotalCost()
			a := agent.New("root", "")
			beforeContext := root.GetMessages(a)
			beforeCompaction, beforeIndices, beforeCount := root.CompactionInput()

			for range 2 {
				for _, event := range []*runtime.EvaluationUsageEvent{
					{SessionID: root.ID, AgentContext: runtime.AgentContext{AgentName: "root"}, Evaluation: rootEvaluation},
					{SessionID: child.ID, AgentContext: runtime.AgentContext{AgentName: "worker"}, Evaluation: childEvaluation},
					{SessionID: root.ID},
				} {
					handled, cmd := p.handleRuntimeEvent(event)
					require.True(t, handled)
					assert.Nil(t, cmd)
				}
			}

			records := root.EvaluationUsageHistorySnapshot()
			require.Equal(t, []*session.Evaluation{rootEvaluation, childEvaluation}, records)
			assert.Equal(t, before, root.MessagesSnapshot())
			assert.InDelta(t, beforeCost, root.TotalCost(), 1e-12, "events must not add cost to persisted items")
			assert.Equal(t, beforeContext, root.GetMessages(a))
			compaction, indices, count := root.CompactionInput()
			assert.Equal(t, beforeCompaction, compaction)
			assert.Equal(t, beforeIndices, indices)
			assert.Equal(t, beforeCount, count)
			input, output := root.Usage()
			assert.Equal(t, int64(100), input)
			assert.Equal(t, int64(20), output)
			assert.Empty(t, root.MessageUsageHistorySnapshot())
			assert.Equal(t, "root", state.CurrentAgentName())
			_, hasCost := state.AgentCost("worker")
			assert.False(t, hasCost, "only cumulative token events update sidebar costs")

			costDialog := dialog.NewCostDialog(root)
			costDialog.SetSize(160, 100)
			assert.Contains(t, ansi.Strip(costDialog.View()), "Total  $0.38", "persisted and transient records must be counted once")

			rootEvaluation.Usage.InputTokens = 999
			*childEvaluation.Cost = 999
			assert.Equal(t, records, root.EvaluationUsageHistorySnapshot(), "event payloads must not alias retained records")
		})
	}
}

func TestEvaluationUsageSnapshotsKeepChatHistoryAndChildContextSeparate(t *testing.T) {
	t.Parallel()

	root, child := session.New(), session.New()
	root.SetUsage(100, 20)
	state := service.NewSessionState(root)
	p := New(animation.NewRuntime(), t.Context(), app.New(t.Context(), queueTestRuntime{}, root), state).(*chatPage)

	for _, tc := range []struct {
		sessionID, agentName string
		input, output        int64
		chatCost, evalCost   float64
	}{
		{root.ID, "root", 100, 20, 0.5, 0.25},
		{child.ID, "worker", 40, 5, 0.125, 0.125},
	} {
		handled, _ := p.handleRuntimeEvent(runtime.NewTokenUsageEvent(tc.sessionID, tc.agentName, &runtime.Usage{
			InputTokens: tc.input, OutputTokens: tc.output, ContextLength: tc.input + tc.output, ContextLimit: 10000,
			Cost: tc.chatCost, LastMessage: &runtime.MessageUsage{
				Usage: chattypes.Usage{InputTokens: tc.input, OutputTokens: tc.output}, Model: "chat-model", Cost: tc.chatCost,
			},
		}))
		require.True(t, handled)
		e := &runtime.EvaluationUsageEvent{
			SessionID: tc.sessionID, AgentContext: runtime.AgentContext{AgentName: tc.agentName},
			Evaluation: &session.Evaluation{
				ID: tc.sessionID + "-evaluation", Evaluator: "safety", AgentName: tc.agentName, Model: "judge-model",
				Usage: &chattypes.Usage{InputTokens: 9000, OutputTokens: 1000}, Cost: new(tc.evalCost),
			},
		}
		snapshot := &runtime.Usage{
			InputTokens: tc.input, OutputTokens: tc.output, ContextLength: tc.input + tc.output, ContextLimit: 10000,
			Cost: tc.chatCost + tc.evalCost, SnapshotOnly: true,
		}
		for range 2 {
			handled, _ = p.handleRuntimeEvent(e)
			require.True(t, handled)
			handled, _ = p.handleRuntimeEvent(runtime.NewTokenUsageEvent(tc.sessionID, tc.agentName, snapshot))
			require.True(t, handled)
		}
		cost, ok := state.AgentCost(tc.agentName)
		require.True(t, ok)
		assert.InDelta(t, tc.chatCost+tc.evalCost, cost, 1e-12)
		usage, ok := state.AgentUsage(tc.agentName)
		require.True(t, ok)
		assert.Equal(t, tc.input+tc.output, usage.ContextLength)
		assert.Nil(t, usage.LastMessage)
	}

	assert.Len(t, root.MessageUsageHistorySnapshot(), 2, "snapshot-only events must not duplicate chat usage")
	assert.Len(t, root.EvaluationUsageHistorySnapshot(), 2)
	assert.Zero(t, root.ItemCount())
	assert.Zero(t, root.TotalCost())
	input, output := root.Usage()
	assert.Equal(t, int64(100), input, "child usage must not overwrite root context")
	assert.Equal(t, int64(20), output)
	costDialog := dialog.NewCostDialog(root)
	costDialog.SetSize(160, 100)
	assert.Contains(t, ansi.Strip(costDialog.View()), "Total  $1.00")
}
