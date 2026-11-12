package runtime

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	agenttool "github.com/docker/docker-agent/pkg/tools/builtin/agent"
)

func TestRunStream_WaitBackgroundAgents(t *testing.T) {
	t.Parallel()

	rootProvider := &queueProvider{id: "test/mock-model", streams: []chat.MessageStream{
		newStreamBuilder().
			AddToolCallName("run_first", agenttool.ToolNameRunBackgroundAgent).
			AddToolCallArguments("run_first", `{"agent":"first","task":"first task"}`).
			AddToolCallName("run_second", agenttool.ToolNameRunBackgroundAgent).
			AddToolCallArguments("run_second", `{"agent":"second","task":"second task"}`).
			AddToolCallStopWithUsage(10, 5).Build(),
		newStreamBuilder().AddContent("dispatched").AddStopWithUsage(10, 5).Build(),
	}}
	first := agent.New("first", "", agent.WithModel(&mockProvider{
		id: "test/mock-model", stream: newStreamBuilder().AddContent("first result").AddStopWithUsage(10, 5).Build(),
	}))
	second := agent.New("second", "", agent.WithModel(&mockProvider{
		id: "test/mock-model", stream: newStreamBuilder().AddContent("second result").AddStopWithUsage(10, 5).Build(),
	}))
	root := agent.New("root", "", agent.WithModel(rootProvider), agent.WithSubAgents(first, second), agent.WithToolSets(agenttool.New()))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root, first, second)),
		WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })

	sess := session.New(session.WithUserMessage("dispatch tasks"), session.WithToolsApproved(true))
	_, err = rt.Run(t.Context(), sess)
	require.NoError(t, err)
	firstID := parseBackgroundTaskID(t, toolResultContent(t, sess, "run_first"))
	secondID := parseBackgroundTaskID(t, toolResultContent(t, sess, "run_second"))

	rootProvider.enqueue(
		newStreamBuilder().
			AddToolCallName("wait_all", agenttool.ToolNameWaitBackgroundAgents).
			AddToolCallArguments("wait_all", fmt.Sprintf(`{"task_ids":[%q,%q],"timeout":10}`, secondID, firstID)).
			AddToolCallStopWithUsage(10, 5).Build(),
		newStreamBuilder().AddContent("joined").AddStopWithUsage(10, 5).Build(),
	)
	sess.AddMessage(session.UserMessage("join both tasks"))
	_, err = rt.Run(t.Context(), sess)
	require.NoError(t, err)

	var result struct {
		AllDone bool `json:"all_done"`
		Tasks   []struct {
			TaskID string `json:"task_id"`
			Status string `json:"status"`
			Output string `json:"output"`
		} `json:"tasks"`
	}
	require.NoError(t, json.Unmarshal([]byte(toolResultContent(t, sess, "wait_all")), &result))
	assert.True(t, result.AllDone)
	require.Len(t, result.Tasks, 2)
	assert.Equal(t, secondID, result.Tasks[0].TaskID)
	assert.Equal(t, "completed", result.Tasks[0].Status)
	assert.Equal(t, "second result", result.Tasks[0].Output)
	assert.Equal(t, firstID, result.Tasks[1].TaskID)
	assert.Equal(t, "completed", result.Tasks[1].Status)
	assert.Equal(t, "first result", result.Tasks[1].Output)
}
