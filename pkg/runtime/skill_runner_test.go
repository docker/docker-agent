package runtime

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/skills"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/handoff"
	skillstool "github.com/docker/docker-agent/pkg/tools/builtin/skills"
	"github.com/docker/docker-agent/pkg/tools/builtin/transfertask"
)

func TestRunSkillFork_SkipsEmbeddedCommandsWithoutParentToolCall(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	marker := filepath.Join(tmpDir, "marker")
	skillFile := filepath.Join(tmpDir, "SKILL.md")
	require.NoError(t, os.WriteFile(skillFile, []byte("Data: !`touch "+marker+"`"), 0o644))

	skillTS := skillstool.New([]skills.Skill{{
		Name:        "gather",
		Description: "Gathers data",
		Context:     "fork",
		FilePath:    skillFile,
		BaseDir:     tmpDir,
		Local:       true,
	}}, tmpDir)
	prov := &queueProvider{id: "test/mock-model", streams: []chat.MessageStream{
		newStreamBuilder().AddContent("done").AddStopWithUsage(1, 1).Build(),
	}}
	a := agent.New("worker", "Worker agent", agent.WithModel(prov), agent.WithToolSets(skillTS))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(a)),
		WithSessionCompaction(false),
		WithModelStore(mockModelStore{}),
	)
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("Test"), session.WithAgentName("worker"))
	result, err := rt.RunSkillFork(t.Context(), sess,
		skillstool.RunSkillArgs{Name: "gather"}, NewChannelSink(make(chan Event, 128)))

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.False(t, result.IsError)
	assert.NoFileExists(t, marker)
	child := firstSubSession(sess)
	require.NotNil(t, child)
	var childContent string
	for _, item := range child.MessagesSnapshot() {
		if item.Message != nil && item.Message.Message.Role == chat.MessageRoleUser {
			childContent = item.Message.Message.Content
			break
		}
	}
	assert.Contains(t, childContent, "cannot ask for approval here")
}

// TestRunSkillFork_PinnedSessionRunsAsPinnedAgent covers fork-mode skills
// invoked from a pinned background session (#3886): the skill lookup, the
// child's identity, and its execution must all resolve from the session's
// pinned agent (worker), never from the shared current agent (root), and
// the shared current agent must stay untouched while the skill child runs.
//
// The pinned session mirrors what RunAgent produces for a background
// delegation root -> worker (pinned to worker, lineage [root]); it is
// constructed directly so worker's scripted provider streams are consumed
// by the skill child alone.
func TestRunSkillFork_PinnedSessionRunsAsPinnedAgent(t *testing.T) {
	t.Parallel()

	var rt *LocalRuntime
	var mu sync.Mutex
	var observed []string
	record := func() {
		mu.Lock()
		defer mu.Unlock()
		observed = append(observed, rt.CurrentAgent().Name())
	}

	// The fork skill is inline (body served from memory), so the test
	// needs no filesystem or command expansion.
	skillTS := skillstool.New([]skills.Skill{{
		Name:          "greet",
		Description:   "Greets the user",
		Context:       "fork",
		InlineContent: "# Greet\nSay the greeting.",
	}}, "")

	// Only worker has the skills toolset. Its provider first makes a
	// probe tool call (recording the shared current agent mid-run), then
	// answers: receiving that answer proves the skill child executed on
	// worker's provider, i.e. as the pinned agent.
	workerProv := &queueProvider{id: "test/mock-model", streams: []chat.MessageStream{
		newStreamBuilder().AddToolCallWithStop("call_probe", "probe", "{}").Build(),
		newStreamBuilder().AddContent("worker skill done").AddStopWithUsage(10, 5).Build(),
	}}
	worker := agent.New("worker", "Worker agent",
		agent.WithModel(workerProv),
		agent.WithToolSets(skillTS, newStubToolSet(nil, probeTool(record), nil)),
	)
	root := agent.New("root", "Root agent", agent.WithModel(&mockProvider{id: "test/mock-model", stream: &mockStream{}}))
	agent.WithSubAgents(worker)(root)

	tm := team.New(team.WithAgents(root, worker))
	var err error
	rt, err = NewLocalRuntime(t.Context(), tm,
		WithSessionCompaction(false),
		WithModelStore(mockModelStore{}),
	)
	require.NoError(t, err)
	require.Equal(t, "root", rt.CurrentAgent().Name(), "shared current agent starts at root")

	sess := session.New(
		session.WithUserMessage("Test"),
		session.WithToolsApproved(true),
		session.WithAgentName("worker"),
		session.WithDelegationLineage([]string{"root"}),
	)

	evts := make(chan Event, 128)
	result, err := rt.RunSkillFork(t.Context(), sess,
		skillstool.RunSkillArgs{Name: "greet", Task: "greet the user"}, NewChannelSink(evts))
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.IsError, "fork skill from a pinned session must succeed: %s", result.Output)
	assert.Equal(t, "worker skill done", result.Output, "the skill child must execute as the pinned caller")

	mu.Lock()
	assert.Equal(t, []string{"root"}, observed,
		"the shared current agent must stay root while the pinned skill child runs")
	mu.Unlock()
	assert.Equal(t, "root", rt.CurrentAgent().Name(), "the shared current agent must remain root afterwards")

	child := firstSubSession(sess)
	require.NotNil(t, child)
	assert.Equal(t, "worker", child.AgentName, "the skill child must be pinned to the pinned caller")
	assert.Equal(t, []string{"root"}, child.DelegationLineage,
		"skills are not delegation edges: lineage must be inherited unchanged, not incremented")

	switches, completed := collectTransferEvents(evts)
	assert.Empty(t, switches, "a pinned skill fork must not emit AgentSwitching events")
	require.NotNil(t, completed)
	assert.Equal(t, "worker", completed.GetAgentName(),
		"SubSessionCompleted must be attributed to the pinned caller")
}

func TestSkillForkHandoffsStaySessionLocal(t *testing.T) {
	t.Parallel()
	for _, forced := range []bool{false, true} {
		name := "explicit"
		if forced {
			name = "forced"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			st := skillstool.New([]skills.Skill{{Name: "test", Context: "fork", InlineContent: "Complete the workflow."}}, "")
			targetModel := &handoffRecordingProvider{mockProvider: mockProvider{id: "test/target", stream: newStreamBuilder().AddContent("target finished").AddStopWithUsage(1, 1).Build()}}
			target := agent.New("target", "", agent.WithModel(targetModel))
			rootStream := newStreamBuilder().AddToolCallWithStop("handoff", "handoff", `{"agent":"target"}`).Build()
			if forced {
				rootStream = newStreamBuilder().AddContent("root finished").AddStopWithUsage(1, 1).Build()
			}
			rootModel := &queueProvider{id: "test/root", streams: []chat.MessageStream{rootStream}}
			opts := []agent.Opt{agent.WithModel(rootModel), agent.WithToolSets(st, handoff.New()), agent.WithHandoffs(target)}
			if forced {
				opts = append(opts, agent.WithForceHandoff(target))
			}
			root := agent.New("root", "", opts...)
			rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root, target)), WithModelStore(mockModelStore{}), WithSessionCompaction(false))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, rt.Close()) })
			parent := session.New(session.WithUserMessage("run workflow"), session.WithToolsApproved(true))
			result, err := rt.RunSkillFork(t.Context(), parent, skillstool.RunSkillArgs{Name: "test"}, NewChannelSink(make(chan Event, 256)))
			require.NoError(t, err)
			require.NotNil(t, result)
			assert.False(t, result.IsError)
			assert.Equal(t, "target finished", result.Output)
			assert.Equal(t, 1, targetModel.handoffCallCount())
			assert.Equal(t, "root", rt.CurrentAgent().Name())
			child := firstSubSession(parent)
			require.NotNil(t, child)
			assert.Equal(t, "root", child.AgentName)
			assert.Equal(t, "target", child.HandoffAgent())
		})
	}
}

func TestHandoffPinnedBackgroundDoesNotSwitchGlobalAgent(t *testing.T) {
	t.Parallel()
	target := agent.New("target", "", agent.WithModel(&mockProvider{id: "test/target"}))
	root := agent.New("root", "", agent.WithModel(&mockProvider{id: "test/root"}), agent.WithHandoffs(target))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root, target)), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	sess := session.New(session.WithAgentName("root"))
	result, err := rt.handleHandoff(t.Context(), sess, tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"agent":"target"}`}}, root)
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Equal(t, "root", rt.CurrentAgent().Name())
	assert.Empty(t, sess.HandoffAgent())
}

func TestSkillDelegatesKeepLocalHandoffs(t *testing.T) {
	t.Parallel()
	for _, forced := range []bool{false, true} {
		name := "explicit"
		if forced {
			name = "forced"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			targetModel := &handoffRecordingProvider{mockProvider: mockProvider{id: "test/target", stream: newStreamBuilder().AddContent("target finished").AddStopWithUsage(1, 1).Build()}}
			target := agent.New("target", "", agent.WithModel(targetModel))
			delegateStream := newStreamBuilder().AddToolCallWithStop("handoff", "handoff", `{"agent":"target"}`).Build()
			if forced {
				delegateStream = newStreamBuilder().AddContent("delegate done").AddStopWithUsage(1, 1).Build()
			}
			opts := []agent.Opt{agent.WithModel(&queueProvider{id: "test/delegate", streams: []chat.MessageStream{delegateStream}}), agent.WithHandoffs(target), agent.WithToolSets(handoff.New())}
			if forced {
				opts = append(opts, agent.WithForceHandoff(target))
			}
			delegate := agent.New("delegate", "", opts...)
			st := skillstool.New([]skills.Skill{{Name: "test", Context: "fork", InlineContent: "Delegate the task."}}, "")
			root := agent.New("root", "", agent.WithModel(&queueProvider{id: "test/root", streams: []chat.MessageStream{
				newStreamBuilder().AddToolCallWithStop("transfer", "transfer_task", `{"agent":"delegate","task":"help"}`).Build(),
				newStreamBuilder().AddContent("root finished").AddStopWithUsage(1, 1).Build(),
			}}), agent.WithSubAgents(delegate), agent.WithToolSets(st, transfertask.New()))
			rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root, delegate, target)), WithModelStore(mockModelStore{}), WithSessionCompaction(false))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, rt.Close()) })
			parent := session.New(session.WithUserMessage("run"), session.WithToolsApproved(true))
			result, err := rt.RunSkillFork(t.Context(), parent, skillstool.RunSkillArgs{Name: "test"}, NewChannelSink(make(chan Event, 512)))
			require.NoError(t, err)
			assert.Equal(t, "root finished", result.Output)
			assert.Equal(t, 1, targetModel.handoffCallCount())
			assert.Equal(t, "root", rt.CurrentAgent().Name())
			child := firstSubSession(parent)
			require.NotNil(t, child)
			nested := firstSubSession(child)
			require.NotNil(t, nested)
			assert.True(t, nested.AllowsAgentHandoffs())
			assert.Equal(t, "target", nested.HandoffAgent())
		})
	}
}
