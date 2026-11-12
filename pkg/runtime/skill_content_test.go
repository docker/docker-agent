package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/js"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/skills"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	skillstool "github.com/docker/docker-agent/pkg/tools/builtin/skills"
)

func TestSkillGuardEntryPoints(t *testing.T) {
	t.Parallel()
	for _, entry := range []string{"read_skill", "read_skill_file", "run_skill", "slash", "fork slash", "legacy command", "JS command"} {
		t.Run(entry, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			marker := filepath.Join(dir, "executed")
			body := "REJECTED_SKILL_BODY !`touch " + marker + "`"
			path := filepath.Join(dir, "SKILL.md")
			require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
			st := skillstool.New([]skills.Skill{{Name: "test", FilePath: path, BaseDir: dir, Local: true, Context: "fork", Files: []string{"SKILL.md", "resource.md"}}}, dir)
			reg := hooks.NewRegistry()
			var seen []hooks.Input
			require.NoError(t, reg.RegisterBuiltin("judge", func(_ context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
				seen = append(seen, *in)
				return &hooks.Output{Decision: "block", Reason: body, SystemMessage: body}, nil
			}))
			worker := agent.New("worker", "", agent.WithModel(&mockProvider{id: "test/mock-model"}), agent.WithToolSets(st), agent.WithHooks(&latest.HooksConfig{SkillContentGuard: []latest.HookDefinition{{Type: "builtin", Command: "judge"}}}), agent.WithCommands(types.Commands{
				"legacy": {Instruction: `!read_skill(name=test)`},
				"js":     {Instruction: `${read_skill_file({skill_name:"test",path:"SKILL.md"})}`},
			}))
			root := agent.New("root", "", agent.WithModel(&mockProvider{id: "test/mock-model"}))
			rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root, worker)), WithHooksRegistry(reg), WithModelStore(mockModelStore{}), WithCommandEvaluatorFactory(func(ts []tools.Tool) CommandEvaluator { return js.NewEvaluator(ts) }))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, rt.Close()) })
			sess := session.New(session.WithAgentName("worker"), session.WithToolsApproved(true))
			evts := make(chan Event, 128)
			sink := NewChannelSink(evts)
			switch entry {
			case "slash":
				content, err := rt.ReadSkillContent(t.Context(), sess, "test")
				require.ErrorContains(t, err, "rejected by policy")
				assert.Empty(t, content)
			case "fork slash":
				result, err := rt.RunSkillFork(t.Context(), sess, skillstool.RunSkillArgs{Name: "test"}, sink)
				require.NoError(t, err)
				require.NotNil(t, result)
				assert.True(t, result.IsError)
				assert.NotContains(t, result.Output, body)
				assert.Nil(t, firstSubSession(sess))
			case "legacy command", "JS command":
				require.NoError(t, rt.SetCurrentAgent(t.Context(), "worker"))
				command := "/legacy"
				if entry == "JS command" {
					command = "/js"
				}
				content := ResolveCommand(t.Context(), rt, command)
				assert.Contains(t, content, "rejected by policy")
				assert.NotContains(t, content, body)
			default:
				ts, err := st.Tools(t.Context())
				require.NoError(t, err)
				args := `{"name":"test"}`
				if entry == "read_skill_file" {
					args = `{"skill_name":"test","path":"SKILL.md"}`
				}
				stopped, _ := rt.processToolCalls(t.Context(), sess, rt.resolveSessionAgent(sess), []tools.ToolCall{{ID: "skill-call", Type: "function", Function: tools.FunctionCall{Name: entry, Arguments: args}}}, ts, sink)
				assert.False(t, stopped)
				assert.Nil(t, firstSubSession(sess))
				assert.Equal(t, 1, sess.MessageCount())
			}
			require.Len(t, seen, 1)
			assert.Equal(t, "worker", seen[0].AgentName)
			assert.Equal(t, body, seen[0].Skill.Content)
			if entry != "legacy command" && entry != "JS command" {
				assert.Equal(t, sess.ID, seen[0].SessionID)
			}
			assert.NoFileExists(t, marker)
			encoded, err := json.Marshal(sess.MessagesSnapshot())
			require.NoError(t, err)
			assert.NotContains(t, string(encoded), "REJECTED_SKILL_BODY")
			close(evts)
			for event := range evts {
				encoded, err := json.Marshal(event)
				require.NoError(t, err)
				assert.NotContains(t, string(encoded), "REJECTED_SKILL_BODY")
			}
		})
	}
}

func TestSkillGuardAllowsWithoutEnablingStandaloneCommands(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	marker := filepath.Join(dir, "executed")
	body := "Instructions !`touch " + marker + "`"
	st := skillstool.New([]skills.Skill{{Name: "test", InlineContent: body}}, dir)
	a := agent.New("root", "", agent.WithModel(&mockProvider{id: "test/mock-model"}), agent.WithToolSets(st), agent.WithHooks(&latest.HooksConfig{SkillContentGuard: []latest.HookDefinition{{Type: "builtin", Command: "allow"}}}))
	reg := hooks.NewRegistry()
	require.NoError(t, reg.RegisterBuiltin("allow", func(context.Context, *hooks.Input, []string) (*hooks.Output, error) {
		return &hooks.Output{Continue: new(true)}, nil
	}))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(a)), WithHooksRegistry(reg), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	content, err := rt.ReadSkillContent(t.Context(), session.New(), "test")
	require.NoError(t, err)
	assert.Contains(t, content, "Instructions")
	assert.Contains(t, content, "cannot ask for approval here")
	assert.NoFileExists(t, marker)
}

type blockingSkillTools struct {
	tools.ToolSet

	started chan struct{}
	resume  chan struct{}
}

func (s *blockingSkillTools) Tools(ctx context.Context) ([]tools.Tool, error) {
	close(s.started)
	select {
	case <-s.resume:
		return s.ToolSet.Tools(ctx)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestStandaloneSkillPolicyPinnedDuringToolDiscovery(t *testing.T) {
	t.Parallel()
	st := skillstool.New([]skills.Skill{{Name: "test", InlineContent: "untrusted"}}, "")
	blocked := &blockingSkillTools{ToolSet: st, started: make(chan struct{}), resume: make(chan struct{})}
	root := agent.New("root", "", agent.WithModel(&mockProvider{id: "test/mock-model"}), agent.WithToolSets(blocked), agent.WithHooks(&latest.HooksConfig{SkillContentGuard: []latest.HookDefinition{{Type: "builtin", Command: "deny"}}}))
	other := agent.New("other", "", agent.WithModel(&mockProvider{id: "test/mock-model"}))
	reg := hooks.NewRegistry()
	require.NoError(t, reg.RegisterBuiltin("deny", func(_ context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
		assert.Equal(t, "root", in.AgentName)
		return &hooks.Output{Decision: "block"}, nil
	}))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root, other)), WithHooksRegistry(reg), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	type listing struct {
		tools []tools.Tool
		err   error
	}
	done := make(chan listing, 1)
	go func() {
		ts, err := rt.CurrentAgentTools(t.Context())
		done <- listing{tools: ts, err: err}
	}()
	<-blocked.started
	require.NoError(t, rt.SetCurrentAgent(t.Context(), "other"))
	close(blocked.resume)
	listed := <-done
	require.NoError(t, listed.err)
	require.NotEmpty(t, listed.tools)
	result, err := listed.tools[0].Handler(t.Context(), tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"name":"test"}`}}, tools.NopRuntime{})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "rejected by policy")
	assert.NotContains(t, result.Output, "untrusted")
}

func TestRunSkillPolicyPinnedBeforeHandler(t *testing.T) {
	t.Parallel()
	rootSkills := skillstool.New([]skills.Skill{{Name: "test", InlineContent: "root content", Context: "fork"}}, "")
	otherSkills := skillstool.New([]skills.Skill{{Name: "test", InlineContent: "other content", Context: "fork"}}, "")
	reg := hooks.NewRegistry()
	entered, resume := make(chan struct{}), make(chan struct{})
	require.NoError(t, reg.RegisterBuiltin("pause", func(ctx context.Context, _ *hooks.Input, _ []string) (*hooks.Output, error) {
		close(entered)
		select {
		case <-resume:
			return &hooks.Output{Continue: new(true)}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}))
	var checked string
	require.NoError(t, reg.RegisterBuiltin("deny", func(_ context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
		checked = in.Skill.Content
		assert.Equal(t, "root", in.AgentName)
		return &hooks.Output{Decision: "block"}, nil
	}))
	root := agent.New("root", "", agent.WithModel(&mockProvider{id: "test/mock-model"}), agent.WithToolSets(rootSkills), agent.WithHooks(&latest.HooksConfig{
		ToolGuard:         []latest.HookMatcherConfig{{Matcher: "run_skill", Hooks: []latest.HookDefinition{{Type: "builtin", Command: "pause"}}}},
		SkillContentGuard: []latest.HookDefinition{{Type: "builtin", Command: "deny"}},
	}))
	other := agent.New("other", "", agent.WithModel(&mockProvider{id: "test/mock-model"}), agent.WithToolSets(otherSkills))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root, other)), WithHooksRegistry(reg), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	sess := session.New(session.WithToolsApproved(true))
	ts, err := rootSkills.Tools(t.Context())
	require.NoError(t, err)
	done := make(chan struct{})
	go func() {
		defer close(done)
		rt.processToolCalls(t.Context(), sess, rt.resolveSessionAgent(sess), []tools.ToolCall{{ID: "fork", Type: "function", Function: tools.FunctionCall{Name: "run_skill", Arguments: `{"name":"test"}`}}}, ts, NewChannelSink(make(chan Event, 128)))
	}()
	<-entered
	require.NoError(t, rt.SetCurrentAgent(t.Context(), "other"))
	close(resume)
	<-done
	assert.Equal(t, "root content", checked)
	assert.Nil(t, firstSubSession(sess))
	assert.Equal(t, 1, sess.MessageCount())
}

func TestAllowedSkillForkKeepsCallerAfterAgentSwitch(t *testing.T) {
	t.Parallel()
	st := skillstool.New([]skills.Skill{{Name: "test", InlineContent: "root instructions", Context: "fork"}}, "")
	rootModel := &queueProvider{id: "test/mock-model", streams: []chat.MessageStream{
		newStreamBuilder().AddContent("root finished").AddStopWithUsage(1, 1).Build(),
	}}
	root := agent.New("root", "", agent.WithModel(rootModel), agent.WithToolSets(st), agent.WithHooks(&latest.HooksConfig{SkillContentGuard: []latest.HookDefinition{{Type: "builtin", Command: "switch"}}, SubagentStop: []latest.HookDefinition{{Type: "builtin", Command: "root-completed"}}}))
	other := agent.New("other", "", agent.WithModel(&mockProvider{id: "test/mock-model"}), agent.WithHooks(&latest.HooksConfig{SubagentStop: []latest.HookDefinition{{Type: "builtin", Command: "other-completed"}}}))
	reg := hooks.NewRegistry()
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root, other)), WithHooksRegistry(reg), WithModelStore(mockModelStore{}), WithSessionCompaction(false))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	require.NoError(t, reg.RegisterBuiltin("switch", func(ctx context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
		assert.Equal(t, "root", in.AgentName)
		assert.NoError(t, rt.SetCurrentAgent(ctx, "other"))
		return &hooks.Output{Continue: new(true)}, nil
	}))
	var completed []string
	for _, name := range []string{"root-completed", "other-completed"} {
		require.NoError(t, reg.RegisterBuiltin(name, func(context.Context, *hooks.Input, []string) (*hooks.Output, error) {
			completed = append(completed, name)
			return nil, nil
		}))
	}
	sess := session.New(session.WithUserMessage("run the skill"))
	result, err := rt.RunSkillFork(t.Context(), sess, skillstool.RunSkillArgs{Name: "test"}, NewChannelSink(make(chan Event, 128)))
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.False(t, result.IsError)
	assert.Equal(t, "root finished", result.Output)
	child := firstSubSession(sess)
	require.NotNil(t, child)
	assert.Equal(t, "root", child.AgentName)
	assert.Equal(t, "other", rt.CurrentAgent().Name())
	assert.Equal(t, []string{"root-completed"}, completed)
}

func TestSkillGuardUsesAgentThatRequestedTool(t *testing.T) {
	t.Parallel()
	for _, toolName := range []string{"read_skill", "read_skill_file", "run_skill"} {
		t.Run(toolName, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			body := "UNTRUSTED_ORIGINAL_AGENT_SKILL"
			require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o600))
			st := skillstool.New([]skills.Skill{{Name: "test", FilePath: filepath.Join(dir, "SKILL.md"), BaseDir: dir, Files: []string{"SKILL.md", "resource.md"}, Context: "fork"}}, dir)
			args := `{"name":"test"}`
			if toolName == "read_skill_file" {
				args = `{"skill_name":"test","path":"SKILL.md"}`
			}
			request := &queueProvider{id: "test/mock-model", streams: []chat.MessageStream{newStreamBuilder().AddToolCallWithStop("load", toolName, args).Build()}}
			otherModel := &handoffRecordingProvider{mockProvider: mockProvider{id: "test/other", stream: newStreamBuilder().AddContent("done").AddStopWithUsage(1, 1).Build()}}
			root := agent.New("root", "", agent.WithModel(request), agent.WithToolSets(st), agent.WithHooks(&latest.HooksConfig{
				AfterLLMCall:      []latest.HookDefinition{{Type: "builtin", Command: "switch"}},
				SkillContentGuard: []latest.HookDefinition{{Type: "builtin", Command: "deny"}},
			}))
			other := agent.New("other", "", agent.WithModel(otherModel))
			rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root, other)), WithModelStore(mockModelStore{}), WithSessionCompaction(false))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, rt.Close()) })
			checks := 0
			require.NoError(t, rt.hooksRegistry.RegisterBuiltin("switch", func(ctx context.Context, _ *hooks.Input, _ []string) (*hooks.Output, error) {
				return nil, rt.SetCurrentAgent(ctx, "other")
			}))
			require.NoError(t, rt.hooksRegistry.RegisterBuiltin("deny", func(_ context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
				checks++
				assert.Equal(t, "root", in.AgentName)
				assert.Equal(t, body, in.Skill.Content)
				return &hooks.Output{Decision: "block"}, nil
			}))
			sess := session.New(session.WithUserMessage("load skill"), session.WithToolsApproved(true))
			for ev := range rt.RunStream(t.Context(), sess) {
				encoded, err := json.Marshal(ev)
				require.NoError(t, err)
				assert.NotContains(t, string(encoded), body)
			}
			assert.Equal(t, 1, checks)
			assert.Nil(t, firstSubSession(sess))
			encoded, err := json.Marshal(sess.MessagesSnapshot())
			require.NoError(t, err)
			assert.NotContains(t, string(encoded), body)
		})
	}
}
