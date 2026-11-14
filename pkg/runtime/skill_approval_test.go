package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/skills"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	skillstool "github.com/docker/docker-agent/pkg/tools/builtin/skills"
	"github.com/docker/docker-agent/pkg/tools/builtin/transfertask"
)

// toolRecorder records the path argument of every tool call. The mutex keeps
// it safe if a future script lets one model turn fan out several calls.
type toolRecorder struct {
	mu    sync.Mutex
	paths []string
}

func (r *toolRecorder) record(tc tools.ToolCall) {
	var args struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)

	r.mu.Lock()
	defer r.mu.Unlock()
	r.paths = append(r.paths, args.Path)
}

func (r *toolRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.paths)
}

func recordedTool(name string, readOnly bool, rec *toolRecorder) tools.Tool {
	return tools.Tool{
		Name:        name,
		Annotations: tools.ToolAnnotations{ReadOnlyHint: readOnly},
		Handler: func(_ context.Context, tc tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
			rec.record(tc)
			return tools.ResultSuccess("ok"), nil
		},
	}
}

// newSkillApprovalRuntime builds a root agent with a fork skill, transfer_task,
// and a sub-agent, all sharing one scripted provider. The skill child runs as
// the root agent, so it inherits the same tool list; the worker gets the same
// tools via its own toolset.
func newSkillApprovalRuntime(
	t *testing.T,
	script []chat.MessageStream,
	skill skills.Skill,
	toolList []tools.Tool,
	opts ...session.Opt,
) (*LocalRuntime, *session.Session) {
	t.Helper()

	prov := &queueProvider{id: "test/mock-model", streams: script}
	worker := agent.New("worker", "Worker agent",
		agent.WithModel(prov),
		agent.WithToolSets(newStubToolSet(nil, toolList, nil)),
	)
	root := agent.New("root", "Root agent",
		agent.WithModel(prov),
		agent.WithSubAgents(worker),
		agent.WithToolSets(
			skillstool.New([]skills.Skill{skill}, ""),
			transfertask.New(),
			newStubToolSet(nil, toolList, nil),
		),
	)

	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root, worker)),
		WithSessionCompaction(false),
		WithModelStore(mockModelStore{}),
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })

	sessionOpts := append([]session.Opt{session.WithUserMessage("start")}, opts...)
	return rt, session.New(sessionOpts...)
}

// runSkillStream drains the run, records every confirmation, and answers it.
// The timeout turns a missing answer into a clear test failure instead of a
// hung test binary.
func runSkillStream(
	t *testing.T,
	rt *LocalRuntime,
	sess *session.Session,
	answer func(*ToolCallConfirmationEvent) ResumeRequest,
) ([]*ToolCallConfirmationEvent, []Event) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	var prompts []*ToolCallConfirmationEvent
	var events []Event
	for ev := range rt.RunStream(ctx, sess) {
		events = append(events, ev)
		prompt, ok := ev.(*ToolCallConfirmationEvent)
		if !ok {
			continue
		}
		prompts = append(prompts, prompt)
		request := ResumeApprove()
		if answer != nil {
			request = answer(prompt)
		}
		rt.Resume(ctx, request)
	}
	require.NoError(t, ctx.Err(), "run timed out; a confirmation was not answered")
	return prompts, events
}

func promptIDs(prompts []*ToolCallConfirmationEvent) []string {
	if len(prompts) == 0 {
		return nil
	}
	ids := make([]string, len(prompts))
	for i, prompt := range prompts {
		ids[i] = prompt.ToolCall.ID
	}
	return ids
}

func reviewSkill() skills.Skill {
	return skills.Skill{
		Name:          "review",
		Description:   "Read-only review",
		Context:       "fork",
		InlineContent: "Review the code, change nothing.",
	}
}

func readOnlySkillScript() []chat.MessageStream {
	return []chat.MessageStream{
		newStreamBuilder().AddToolCallWithStop("root-skill", "run_skill", `{"name":"review","task":"review"}`).Build(),
		newStreamBuilder().AddToolCallWithStop("child-read", "read_file", `{"path":"child"}`).Build(),
		newStreamBuilder().AddToolCallWithStop("child-transfer", "transfer_task", `{"agent":"worker","task":"help"}`).Build(),
		newStreamBuilder().AddToolCallWithStop("worker-read", "read_file", `{"path":"worker"}`).Build(),
		newStreamBuilder().AddContent("worker done").AddStopWithUsage(1, 1).Build(),
		newStreamBuilder().AddContent("skill done").AddStopWithUsage(1, 1).Build(),
		newStreamBuilder().AddContent("root done").AddStopWithUsage(1, 1).Build(),
	}
}

// TestForkSkillReadOnlyApprovalMatrix pins the wrapper classification across
// every safety mode. The wrapper is a delegation, so only strict should ask
// about it; strict must still ask about every nested call too.
func TestForkSkillReadOnlyApprovalMatrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		policy      session.SafetyPolicy
		wantPrompts []string
	}{
		{name: "legacy", policy: ""},
		{name: "balanced", policy: session.SafetyPolicyBalanced},
		{name: "restricted", policy: session.SafetyPolicyRestricted},
		{name: "autonomous", policy: session.SafetyPolicyAutonomous},
		{
			name:        "strict",
			policy:      session.SafetyPolicyStrict,
			wantPrompts: []string{"root-skill", "child-read", "child-transfer", "worker-read"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			readCalls := &toolRecorder{}
			rt, sess := newSkillApprovalRuntime(t, readOnlySkillScript(), reviewSkill(),
				[]tools.Tool{recordedTool("read_file", true, readCalls)},
				session.WithSafetyPolicy(tc.policy),
			)

			prompts, _ := runSkillStream(t, rt, sess, nil)

			assert.Equal(t, tc.wantPrompts, promptIDs(prompts))
			assert.Equal(t, []string{"child", "worker"}, readCalls.snapshot())
			assert.Equal(t, "root done", sess.GetLastAssistantMessageContent())

			skillChild := firstSubSession(sess)
			require.NotNil(t, skillChild, "fork skill must create a sub-session")
			assert.Equal(t, tc.policy, skillChild.GetSafetyPolicy())

			transferChild := firstSubSession(skillChild)
			require.NotNil(t, transferChild, "transfer_task inside the fork must create a nested sub-session")
			assert.Equal(t, tc.policy, transferChild.GetSafetyPolicy())
			assert.Equal(t, "worker", transferChild.AgentName)
		})
	}
}

// TestForkSkillDoesNotAutoApproveNestedWrites is the negative control for the
// wrapper hint: launching the fork is safe, a write inside it is not. Legacy
// and balanced are the two modes where the wrapper hint changes the launch
// decision, so both must still ask about the write.
func TestForkSkillDoesNotAutoApproveNestedWrites(t *testing.T) {
	t.Parallel()

	for _, policy := range []session.SafetyPolicy{"", session.SafetyPolicyBalanced} {
		name := string(policy)
		if name == "" {
			name = "legacy"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			writeCalls := &toolRecorder{}
			rt, sess := newSkillApprovalRuntime(t, []chat.MessageStream{
				newStreamBuilder().AddToolCallWithStop("root-skill", "run_skill", `{"name":"review","task":"review"}`).Build(),
				newStreamBuilder().AddToolCallWithStop("child-write", "write_file", `{"path":"child"}`).Build(),
				newStreamBuilder().AddContent("skill done").AddStopWithUsage(1, 1).Build(),
				newStreamBuilder().AddContent("root done").AddStopWithUsage(1, 1).Build(),
			}, reviewSkill(), []tools.Tool{recordedTool("write_file", false, writeCalls)},
				session.WithSafetyPolicy(policy),
			)

			prompts, _ := runSkillStream(t, rt, sess, nil)

			require.Len(t, prompts, 1, "the child's write must still be confirmed")
			assert.Equal(t, "write_file", prompts[0].ToolCall.Function.Name)
			assert.Equal(t, []string{"child"}, writeCalls.snapshot())
			assert.Equal(t, "root done", sess.GetLastAssistantMessageContent())
		})
	}
}

// TestForkSkillRestrictedModeStillDeniesNestedWrites shows the wrapper hint
// cannot widen the child's policy: restricted mode still denies an unknown
// tool call inside the fork, without asking.
func TestForkSkillRestrictedModeStillDeniesNestedWrites(t *testing.T) {
	t.Parallel()

	writeCalls := &toolRecorder{}
	rt, sess := newSkillApprovalRuntime(t, []chat.MessageStream{
		newStreamBuilder().AddToolCallWithStop("root-skill", "run_skill", `{"name":"review","task":"review"}`).Build(),
		newStreamBuilder().AddToolCallWithStop("child-write", "write_file", `{"path":"child"}`).Build(),
		newStreamBuilder().AddContent("skill done").AddStopWithUsage(1, 1).Build(),
		newStreamBuilder().AddContent("root done").AddStopWithUsage(1, 1).Build(),
	}, reviewSkill(), []tools.Tool{recordedTool("write_file", false, writeCalls)},
		session.WithSafetyPolicy(session.SafetyPolicyRestricted),
	)

	prompts, events := runSkillStream(t, rt, sess, nil)

	assert.Empty(t, prompts, "restricted mode denies instead of prompting")
	assert.Empty(t, writeCalls.snapshot(), "the child's write must not run")

	var denied *ToolCallResponseEvent
	for _, event := range events {
		if response, ok := event.(*ToolCallResponseEvent); ok && response.ToolCallID == "child-write" {
			denied = response
		}
	}
	require.NotNil(t, denied, "the child must receive a response for its write")
	require.NotNil(t, denied.Result)
	assert.True(t, denied.Result.IsError)
	assert.Contains(t, denied.Response, "restricted safety mode")
	assert.Equal(t, "root done", sess.GetLastAssistantMessageContent())
}

// TestForkSkillChildApprovalDoesNotEscapeToParent pins the least-privilege
// boundary: an approve-autonomous decision inside a fork covers that fork and
// its descendants, but not the parent or its later calls.
func TestForkSkillChildApprovalDoesNotEscapeToParent(t *testing.T) {
	t.Parallel()

	writeCalls := &toolRecorder{}
	rt, sess := newSkillApprovalRuntime(t, []chat.MessageStream{
		newStreamBuilder().AddToolCallWithStop("root-skill", "run_skill", `{"name":"review","task":"review"}`).Build(),
		newStreamBuilder().AddToolCallWithStop("child-write", "write_file", `{"path":"child"}`).Build(),
		newStreamBuilder().AddToolCallWithStop("child-transfer", "transfer_task", `{"agent":"worker","task":"help"}`).Build(),
		newStreamBuilder().AddToolCallWithStop("worker-write", "write_file", `{"path":"worker"}`).Build(),
		newStreamBuilder().AddContent("worker done").AddStopWithUsage(1, 1).Build(),
		newStreamBuilder().AddContent("skill done").AddStopWithUsage(1, 1).Build(),
		newStreamBuilder().AddToolCallWithStop("root-write", "write_file", `{"path":"root"}`).Build(),
		newStreamBuilder().AddContent("root done").AddStopWithUsage(1, 1).Build(),
	}, reviewSkill(), []tools.Tool{recordedTool("write_file", false, writeCalls)})

	prompts, _ := runSkillStream(t, rt, sess, func(prompt *ToolCallConfirmationEvent) ResumeRequest {
		if prompt.ToolCall.ID == "child-write" {
			return ResumeApproveAutonomous()
		}
		return ResumeApprove()
	})

	assert.Equal(t, []string{"child-write", "root-write"}, promptIDs(prompts))
	assert.Equal(t, []string{"child", "worker", "root"}, writeCalls.snapshot())
	assert.Empty(t, sess.GetSafetyPolicy(), "the child's grant must not change the parent")
	assert.Equal(t, "root done", sess.GetLastAssistantMessageContent())

	skillChild := firstSubSession(sess)
	require.NotNil(t, skillChild)
	assert.Equal(t, session.SafetyPolicyAutonomous, skillChild.GetSafetyPolicy())

	transferChild := firstSubSession(skillChild)
	require.NotNil(t, transferChild)
	assert.Equal(t, session.SafetyPolicyAutonomous, transferChild.GetSafetyPolicy())
	assert.Equal(t, "worker", transferChild.AgentName)
}

// TestForkSkillInheritsParentSessionPermissions covers the scoped reuse that
// already works: a session-level allow grant follows the fork into its child.
func TestForkSkillInheritsParentSessionPermissions(t *testing.T) {
	t.Parallel()

	writeCalls := &toolRecorder{}
	rt, sess := newSkillApprovalRuntime(t, []chat.MessageStream{
		newStreamBuilder().AddToolCallWithStop("root-skill", "run_skill", `{"name":"review","task":"review"}`).Build(),
		newStreamBuilder().AddToolCallWithStop("child-allowed", "write_file", `{"path":"allowed/a"}`).Build(),
		newStreamBuilder().AddToolCallWithStop("child-blocked", "write_file", `{"path":"blocked/b"}`).Build(),
		newStreamBuilder().AddContent("skill done").AddStopWithUsage(1, 1).Build(),
		newStreamBuilder().AddContent("root done").AddStopWithUsage(1, 1).Build(),
	}, reviewSkill(), []tools.Tool{recordedTool("write_file", false, writeCalls)},
		session.WithPermissions(&session.PermissionsConfig{
			Allow: []string{"write_file:path=allowed/*"},
		}),
	)

	prompts, _ := runSkillStream(t, rt, sess, nil)

	require.Len(t, prompts, 1, "only the path outside the inherited allow rule may prompt")
	assert.Equal(t, "child-blocked", prompts[0].ToolCall.ID)
	assert.Equal(t, []string{"allowed/a", "blocked/b"}, writeCalls.snapshot())
	assert.Equal(t, "root done", sess.GetLastAssistantMessageContent())
}

// TestForkSkillEmbeddedCommandStillGated proves the wrapper hint does not
// bypass ConfirmAndRun for commands embedded in a skill body.
func TestForkSkillEmbeddedCommandStillGated(t *testing.T) {
	t.Parallel()
	if goruntime.GOOS == "windows" {
		t.Skip("skill command expansion uses a POSIX shell")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "SKILL.md")
	require.NoError(t, os.WriteFile(path, []byte("Info: !`echo hello`"), 0o644))

	rt, sess := newSkillApprovalRuntime(t, []chat.MessageStream{
		newStreamBuilder().AddToolCallWithStop("root-skill", "run_skill", `{"name":"expand","task":"expand"}`).Build(),
		newStreamBuilder().AddContent("skill done").AddStopWithUsage(1, 1).Build(),
		newStreamBuilder().AddContent("root done").AddStopWithUsage(1, 1).Build(),
	}, skills.Skill{
		Name:        "expand",
		Description: "Expand an embedded command",
		Context:     "fork",
		FilePath:    path,
		BaseDir:     dir,
		Local:       true,
	}, []tools.Tool{recordedTool("read_file", true, &toolRecorder{})})

	prompts, _ := runSkillStream(t, rt, sess, nil)

	require.Len(t, prompts, 1)
	assert.Equal(t, "shell", prompts[0].ToolCall.Function.Name,
		"the wrapper hint must not silence the skill-body command gate")
	assert.Equal(t, "root done", sess.GetLastAssistantMessageContent())

	skillChild := firstSubSession(sess)
	require.NotNil(t, skillChild)
	var userMessage string
	for _, item := range skillChild.MessagesSnapshot() {
		if item.Message != nil && item.Message.Message.Role == chat.MessageRoleUser {
			userMessage = item.Message.Message.Content
			break
		}
	}
	assert.Contains(t, userMessage, "hello")
}
