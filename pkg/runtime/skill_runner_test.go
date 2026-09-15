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

// TestRunSkillFork_MixedBatchWithTransferRunsAsTheCaller closes #4156's last
// unguarded shape: a batch of [transfer_task, run_skill]. The delegation saw no
// sibling of its own name, counted itself solo and swapped the shared current
// agent to its target; the skill child, left unpinned, then resolved from that
// same field and ran as the transfer's target instead of as its caller.
//
// A fork skill always knows its caller — runSkillFork resolves it before
// dispatch — so the child is pinned to it and never consults the shared field,
// whatever the siblings do.
func TestRunSkillFork_MixedBatchWithTransferRunsAsTheCaller(t *testing.T) {
	t.Parallel()

	skillTS := skillstool.New([]skills.Skill{{
		Name:          "greet",
		Description:   "Greets the user",
		Context:       "fork",
		InlineContent: "# Greet\nSay the greeting.",
	}}, "")

	// Both agents answer with their own name, so the skill result names the
	// agent the child actually executed as.
	workerProv := &queueProvider{id: "test/mock-model", streams: []chat.MessageStream{
		newStreamBuilder().AddContent("worker done").AddStopWithUsage(10, 5).Build(),
		newStreamBuilder().AddContent("skill ran as worker").AddStopWithUsage(10, 5).Build(),
	}}
	worker := agent.New("worker", "Worker agent", agent.WithModel(workerProv))

	// One assistant response carrying both calls: the delegation and the fork.
	batch := newStreamBuilder().
		AddToolCallName("call_transfer", transfertask.ToolNameTransferTask).
		AddToolCallArguments("call_transfer", `{"agent":"worker","task":"chunk","expected_output":"result"}`).
		AddToolCallName("call_skill", skillstool.ToolNameRunSkill).
		AddToolCallArguments("call_skill", `{"name":"greet","task":"greet the user"}`)
	rootProv := &queueProvider{id: "test/mock-model", streams: []chat.MessageStream{
		batch.AddToolCallStopWithUsage(10, 5).Build(),
		newStreamBuilder().AddContent("skill ran as root").AddStopWithUsage(10, 5).Build(),
		newStreamBuilder().AddContent("root done").AddStopWithUsage(10, 5).Build(),
	}}
	root := agent.New("root", "Root agent",
		agent.WithModel(rootProv),
		agent.WithSubAgents(worker),
		agent.WithToolSets(transfertask.New(), skillTS),
	)

	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root, worker)),
		WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close() })

	sess := session.New(session.WithUserMessage("split and skill"), session.WithToolsApproved(true))
	_, err = rt.Run(t.Context(), sess)
	require.NoError(t, err)

	assert.Equal(t, "skill ran as root", toolResultContent(t, sess, "call_skill"),
		"the fork skill must run as its caller, not as the sibling delegation's target")
	assert.Equal(t, "worker done", toolResultContent(t, sess, "call_transfer"),
		"the delegation must still run as its own target")
}
