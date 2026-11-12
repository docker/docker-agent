package runtime

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	agenttool "github.com/docker/docker-agent/pkg/tools/builtin/agent"
	"github.com/docker/docker-agent/pkg/tools/builtin/transfertask"
)

func TestBackgroundDrainPreservesNestedUsage(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	contentSeen, canceled := make(chan struct{}), make(chan struct{})
	leaf := agent.New("leaf", "test", agent.WithModel(&mockProvider{id: "test/mock-model", stream: newStreamBuilder().
		AddContent("leaf partial").AddToolCallName("cancel", "cancel_task").AddToolCallArguments("cancel", "{}").AddToolCallStopWithUsage(100, 50).Build()}),
		agent.WithTools(tools.Tool{Name: "cancel_task", Parameters: map[string]any{}, Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			<-contentSeen
			cancel()
			close(canceled)
			return tools.ResultSuccess("canceled"), nil
		}}))
	worker := agent.New("worker", "test", agent.WithSubAgents(leaf), agent.WithToolSets(transfertask.New()), agent.WithModel(&mockProvider{id: "test/mock-model", stream: newStreamBuilder().
		AddToolCallName("transfer", "transfer_task").AddToolCallArguments("transfer", `{"agent":"leaf","task":"work"}`).AddToolCallStopWithUsage(50, 10).Build()}))
	root := agent.New("root", "test", agent.WithSubAgents(worker), agent.WithModel(&mockProvider{id: "test/mock-model", stream: &mockStream{}}))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root, worker, leaf)), WithSessionCompaction(false), WithModelStore(mockModelStoreWithCostAndLimit{limit: 100_000, cost: modelsdev.Cost{Input: 10, Output: 20}}))
	require.NoError(t, err)
	defer func() { require.NoError(t, rt.Close()) }()
	snapshots := map[string]*Usage{}
	rt.OnBackgroundEvent(func(event Event) {
		if usage, ok := event.(*TokenUsageEvent); ok {
			snapshots[usage.SessionID] = usage.Usage
		}
	})
	parent := session.New(session.WithToolsApproved(true))
	var once sync.Once
	rt.RunAgent(ctx, agenttool.RunParams{AgentName: "worker", Task: "nested", ParentSession: parent, OnContent: func(string) { once.Do(func() { close(contentSeen); <-canceled }) }})
	child := lastAttachedSubSession(parent)
	require.NotNil(t, child)
	grandchild := lastAttachedSubSession(child)
	require.NotNil(t, grandchild)
	require.Contains(t, snapshots, grandchild.ID, "grandchild usage buffered behind the canceled content callback must survive drain")
	require.Contains(t, snapshots, child.ID)
	assert.InDelta(t, grandchild.OwnCost(), snapshots[grandchild.ID].Cost, 1e-9)
	assert.InDelta(t, parent.TotalCost(), snapshots[child.ID].Cost+snapshots[grandchild.ID].Cost, 1e-9)
}
