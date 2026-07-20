package runtime

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/permissions"
	"github.com/docker/docker-agent/pkg/runtime/toolexec"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	agenttool "github.com/docker/docker-agent/pkg/tools/builtin/agent"
)

func TestBuildTaskSystemMessage(t *testing.T) {
	t.Parallel()

	t.Run("with expected output", func(t *testing.T) {
		msg := buildTaskSystemMessage("do the thing", "a result", nil)
		assert.Contains(t, msg, "<task>\ndo the thing\n</task>")
		assert.Contains(t, msg, "<expected_output>\na result\n</expected_output>")
		assert.NotContains(t, msg, "<attached_files>")
	})

	t.Run("without expected output", func(t *testing.T) {
		msg := buildTaskSystemMessage("do the thing", "", nil)
		assert.Contains(t, msg, "<task>\ndo the thing\n</task>")
		assert.NotContains(t, msg, "expected_output")
		assert.NotContains(t, msg, "<attached_files>")
	})

	t.Run("with attached files", func(t *testing.T) {
		fooPath, _ := filepath.Abs("/abs/foo.go")
		barPath, _ := filepath.Abs("/abs/bar.go")
		msg := buildTaskSystemMessage("do the thing", "", []string{fooPath, barPath})
		assert.Contains(t, msg, "<task>\ndo the thing\n</task>")
		assert.Contains(t, msg, "<attached_files>\n- "+fooPath+"\n- "+barPath+"\n</attached_files>")
	})
}

func TestAgentNames(t *testing.T) {
	t.Parallel()

	agents := []*agent.Agent{
		agent.New("alpha", ""),
		agent.New("beta", ""),
	}
	assert.Equal(t, []string{"alpha", "beta"}, agentNames(agents))
	assert.Empty(t, agentNames(nil))
}

func TestValidateAgentInList(t *testing.T) {
	t.Parallel()

	agents := []*agent.Agent{
		agent.New("sub1", ""),
		agent.New("sub2", ""),
	}

	t.Run("valid agent returns nil", func(t *testing.T) {
		result := validateAgentInList("root", "sub1", "transfer to", "sub-agents", agents)
		assert.Nil(t, result)
	})

	t.Run("invalid agent with non-empty list", func(t *testing.T) {
		result := validateAgentInList("root", "missing", "transfer to", "sub-agents", agents)
		require.NotNil(t, result)
		assert.True(t, result.IsError)
		assert.Contains(t, result.Output, "sub1")
		assert.Contains(t, result.Output, "sub2")
	})

	t.Run("invalid agent with empty list", func(t *testing.T) {
		result := validateAgentInList("root", "missing", "transfer to", "sub-agents", nil)
		require.NotNil(t, result)
		assert.True(t, result.IsError)
		assert.Contains(t, result.Output, "No agents are configured")
	})
}

func TestNewSubSession(t *testing.T) {
	t.Parallel()

	parent := session.New(session.WithUserMessage("hello"))
	childAgent := agent.New("worker", "a worker agent",
		agent.WithMaxIterations(10),
	)

	t.Run("basic config", func(t *testing.T) {
		cfg := SubSessionConfig{
			Task:           "write tests",
			ExpectedOutput: "passing tests",
			AgentName:      "worker",
			Title:          "Test task",
			ToolsApproved:  true,
		}

		s := newSubSession(parent, cfg, childAgent)

		assert.Equal(t, parent.ID, s.ParentID)
		assert.Equal(t, "Test task", s.Title)
		assert.True(t, s.ToolsApproved)
		assert.False(t, s.SendUserMessage)
		assert.Equal(t, 10, s.MaxIterations)
		// AgentName should NOT be set when PinAgent is false
		assert.Empty(t, s.AgentName)
	})

	t.Run("pin agent", func(t *testing.T) {
		cfg := SubSessionConfig{
			Task:      "background work",
			AgentName: "worker",
			Title:     "Background task",
			PinAgent:  true,
		}

		s := newSubSession(parent, cfg, childAgent)

		assert.Equal(t, "worker", s.AgentName)
	})

	t.Run("custom implicit user message", func(t *testing.T) {
		cfg := SubSessionConfig{
			Task:                "bump deps",
			AgentName:           "worker",
			Title:               "Skill task",
			ImplicitUserMessage: "Update all Go dependencies",
		}

		s := newSubSession(parent, cfg, childAgent)

		// The implicit user message should be the custom one, not "Please proceed."
		assert.Equal(t, "Update all Go dependencies", s.GetLastUserMessageContent())
	})

	t.Run("default implicit user message", func(t *testing.T) {
		cfg := SubSessionConfig{
			Task:      "do work",
			AgentName: "worker",
			Title:     "Task",
		}

		s := newSubSession(parent, cfg, childAgent)

		assert.Equal(t, "Please proceed.", s.GetLastUserMessageContent())
	})

	t.Run("custom system message", func(t *testing.T) {
		cfg := SubSessionConfig{
			Task:          "bump deps",
			SystemMessage: "You are a skill sub-agent. Follow these instructions.",
			AgentName:     "worker",
			Title:         "Skill task",
		}

		s := newSubSession(parent, cfg, childAgent)

		// When SystemMessage is set, the default task-based message should not be used.
		// We can verify the user message is still the default.
		assert.Equal(t, "Please proceed.", s.GetLastUserMessageContent())
	})
}

func TestSubSessionConfig_DefaultValues(t *testing.T) {
	t.Parallel()

	// Verify zero-value SubSessionConfig produces a valid session
	parent := session.New(session.WithUserMessage("hello"))
	childAgent := agent.New("worker", "")

	cfg := SubSessionConfig{
		Task:      "minimal task",
		AgentName: "worker",
		Title:     "Minimal",
	}

	s := newSubSession(parent, cfg, childAgent)

	assert.False(t, s.ToolsApproved)
	assert.False(t, s.SendUserMessage)
	assert.Empty(t, s.AgentName)
}

func TestSubSessionConfig_InheritsAgentLimits(t *testing.T) {
	t.Parallel()

	parent := session.New(session.WithUserMessage("hello"))

	t.Run("with custom limits", func(t *testing.T) {
		childAgent := agent.New("worker", "",
			agent.WithMaxIterations(42),
			agent.WithMaxConsecutiveToolCalls(7),
		)

		cfg := SubSessionConfig{
			Task:      "work",
			AgentName: "worker",
			Title:     "test",
		}

		s := newSubSession(parent, cfg, childAgent)
		assert.Equal(t, 42, s.MaxIterations)
		assert.Equal(t, 7, s.MaxConsecutiveToolCalls)
	})

	t.Run("with zero limits (defaults)", func(t *testing.T) {
		childAgent := agent.New("worker", "")

		cfg := SubSessionConfig{
			Task:      "work",
			AgentName: "worker",
			Title:     "test",
		}

		s := newSubSession(parent, cfg, childAgent)
		assert.Equal(t, 0, s.MaxIterations)
		assert.Equal(t, 0, s.MaxConsecutiveToolCalls)
	})
}

func TestSubSessionInheritsAttachedFiles(t *testing.T) {
	t.Parallel()

	fooPath, _ := filepath.Abs("/abs/foo.go")
	barPath, _ := filepath.Abs("/abs/bar.go")

	parent := session.New(session.WithUserMessage("hello"))
	parent.AddAttachedFile(fooPath)
	parent.AddAttachedFile(barPath)
	parent.AddAttachedFile(fooPath) // duplicate, should be ignored

	childAgent := agent.New("worker", "")
	cfg := SubSessionConfig{
		Task:      "refactor",
		AgentName: "worker",
		Title:     "Refactor",
	}

	s := newSubSession(parent, cfg, childAgent)

	// Child session inherits parent's attached files (deduplicated, ordered).
	assert.Equal(t, []string{fooPath, barPath}, s.AttachedFilesSnapshot())

	// The system message lists them so the sub-agent sees them up-front.
	sysMsg := s.GetMessages(childAgent)
	require.NotEmpty(t, sysMsg)
	var joined strings.Builder
	for _, m := range sysMsg {
		joined.WriteString(m.Content)
		joined.WriteString("\n")
	}
	assert.Contains(t, joined.String(), "<attached_files>\n- "+fooPath+"\n- "+barPath+"\n</attached_files>")
}

func TestSubSessionWithoutAttachedFilesOmitsBlock(t *testing.T) {
	t.Parallel()

	parent := session.New(session.WithUserMessage("hello"))
	childAgent := agent.New("worker", "")
	cfg := SubSessionConfig{
		Task:      "refactor",
		AgentName: "worker",
		Title:     "Refactor",
	}

	s := newSubSession(parent, cfg, childAgent)
	assert.Empty(t, s.AttachedFilesSnapshot())

	msgs := s.GetMessages(childAgent)
	require.NotEmpty(t, msgs)
	for _, m := range msgs {
		assert.NotContains(t, m.Content, "<attached_files>")
	}
}

func TestSubSessionInheritsPermissions(t *testing.T) {
	t.Parallel()

	perms := &session.PermissionsConfig{
		Allow: []string{"read_*"},
		Deny:  []string{"write_*"},
		Ask:   []string{"edit_*"},
	}
	parent := session.New(session.WithPermissions(perms))

	childAgent := agent.New("worker", "")
	cfg := SubSessionConfig{
		Task:        "refactor",
		AgentName:   "worker",
		Title:       "Refactor",
		Permissions: parent.ClonePermissions(),
	}

	s := newSubSession(parent, cfg, childAgent)

	require.NotNil(t, s.Permissions)
	assert.Equal(t, perms.Allow, s.Permissions.Allow)
	assert.Equal(t, perms.Deny, s.Permissions.Deny)
	assert.Equal(t, perms.Ask, s.Permissions.Ask)

	// Even with ToolsApproved set (yolo), an inherited Deny must win during dispatch.
	s.ToolsApproved = true

	checker := permissions.NewChecker(&latest.PermissionsConfig{
		Allow: s.Permissions.Allow,
		Ask:   s.Permissions.Ask,
		Deny:  s.Permissions.Deny,
	})
	namedCheckers := []toolexec.NamedChecker{
		{Checker: checker, Source: "session permissions"},
	}

	decision := toolexec.Decide(s.ToolsApproved, namedCheckers, "write_file", map[string]any{"path": "foo"}, false)
	assert.Equal(t, toolexec.OutcomeDeny, decision.Outcome, "Inherited Deny should override ToolsApproved: true (yolo)")
}

func TestNewSubSession_PermissionsIsolation(t *testing.T) {
	t.Parallel()

	parent := session.New(session.WithUserMessage("hello"))
	childAgent := agent.New("worker", "")

	t.Run("cloned from config", func(t *testing.T) {
		perms := &session.PermissionsConfig{
			Allow: []string{"read_file"},
		}

		cfg := SubSessionConfig{
			Task:        "isolated work",
			AgentName:   "worker",
			Title:       "Task",
			Permissions: perms,
		}

		s := newSubSession(parent, cfg, childAgent)

		require.NotNil(t, s.Permissions)
		assert.Equal(t, []string{"read_file"}, s.Permissions.Allow)

		perms.Allow = append(perms.Allow, "write_file")

		assert.Equal(t, []string{"read_file"}, s.Permissions.Allow)
	})

	t.Run("nil permissions", func(t *testing.T) {
		cfg := SubSessionConfig{
			Task:      "work without permissions",
			AgentName: "worker",
			Title:     "Task",
		}

		s := newSubSession(parent, cfg, childAgent)
		assert.Nil(t, s.Permissions)
	})
}

func TestSession_ClonePermissions(t *testing.T) {
	t.Parallel()

	t.Run("returns deep copy", func(t *testing.T) {
		perms := &session.PermissionsConfig{
			Allow: []string{"read_file"},
			Deny:  []string{"write_file"},
		}
		s := session.New(session.WithPermissions(perms))

		cloned := s.ClonePermissions()
		require.NotNil(t, cloned)
		assert.Equal(t, perms.Allow, cloned.Allow)
		assert.Equal(t, perms.Deny, cloned.Deny)

		cloned.Allow = append(cloned.Allow, "exec_command")
		original := s.ClonePermissions()
		assert.Equal(t, []string{"read_file"}, original.Allow)
	})

	t.Run("returns nil when unset", func(t *testing.T) {
		s := session.New()
		assert.Nil(t, s.ClonePermissions())
	})
}

func TestSession_SetPermissions(t *testing.T) {
	t.Parallel()

	s := session.New()
	assert.Nil(t, s.ClonePermissions())

	perms := &session.PermissionsConfig{
		Allow: []string{"read_file"},
	}
	s.SetPermissions(perms)

	got := s.ClonePermissions()
	require.NotNil(t, got)
	assert.Equal(t, []string{"read_file"}, got.Allow)
}

func TestRunAgent_InheritsParentPermissions(t *testing.T) {
	t.Parallel()

	workerStream := newStreamBuilder().AddContent("done").AddStopWithUsage(10, 5).Build()
	parentProv := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	workerProv := &mockProvider{id: "test/mock-model", stream: workerStream}

	worker := agent.New("worker", "Worker agent", agent.WithModel(workerProv))
	root := agent.New("root", "Root agent", agent.WithModel(parentProv))
	agent.WithSubAgents(worker)(root)

	tm := team.New(team.WithAgents(root, worker))
	rt, err := NewLocalRuntime(t.Context(), tm,
		WithSessionCompaction(false),
		WithModelStore(mockModelStore{}),
	)
	require.NoError(t, err)

	parentPerms := &session.PermissionsConfig{
		Allow: []string{"read_file", "list_dir"},
		Deny:  []string{"shell:cmd=rm*"},
	}
	parentSession := session.New(
		session.WithUserMessage("Test"),
		session.WithToolsApproved(true),
		session.WithPermissions(parentPerms),
	)

	result := rt.RunAgent(t.Context(), agenttool.RunParams{
		AgentName:     "worker",
		Task:          "do something",
		ParentSession: parentSession,
	})
	require.Empty(t, result.ErrMsg, "RunAgent should succeed")

	var childSession *session.Session
	for _, item := range parentSession.Messages {
		if item.SubSession != nil {
			childSession = item.SubSession
			break
		}
	}
	require.NotNil(t, childSession, "parent must have a sub-session")

	assert.True(t, childSession.ToolsApproved,
		"child session must inherit ToolsApproved from parent")

	require.NotNil(t, childSession.Permissions)
	assert.Equal(t, []string{"read_file", "list_dir"}, childSession.Permissions.Allow)
	assert.Equal(t, []string{"shell:cmd=rm*"}, childSession.Permissions.Deny)

	childSession.Permissions.Allow = append(childSession.Permissions.Allow, "write_file")
	parentClone := parentSession.ClonePermissions()
	assert.Equal(t, []string{"read_file", "list_dir"}, parentClone.Allow,
		"parent permissions must be isolated from child mutations")
}

// TestRunForwarding_DoesNotBackPropagateApprovals locks the "permissions only
// flow downwards" invariant: approvals granted within a sub-session scope must
// not escalate the parent's ToolsApproved gate or permission rules.
func TestRunForwarding_DoesNotBackPropagateApprovals(t *testing.T) {
	t.Parallel()

	childStream := newStreamBuilder().AddContent("done").AddStopWithUsage(10, 5).Build()
	prov := &mockProvider{id: "test/mock-model", stream: childStream}

	librarian := agent.New("librarian", "Library agent", agent.WithModel(prov))
	root := agent.New("root", "Root agent", agent.WithModel(prov))
	agent.WithSubAgents(librarian)(root)

	tm := team.New(team.WithAgents(root, librarian))
	rt, err := NewLocalRuntime(t.Context(), tm,
		WithSessionCompaction(false),
		WithModelStore(mockModelStore{}),
	)
	require.NoError(t, err)

	parent := session.New(
		session.WithUserMessage("Test"),
		session.WithPermissions(&session.PermissionsConfig{Deny: []string{"dangerous_tool"}}),
	)
	require.False(t, parent.IsToolsApproved())

	evts := make(chan Event, 128)
	// Child scope broader than the parent's, as if the user had clicked
	// "approve all" / "always allow" inside the sub-session.
	_, err = rt.runForwarding(t.Context(), parent, NewChannelSink(evts), delegationRequest{
		SubSessionConfig: SubSessionConfig{
			Task:          "find a book",
			AgentName:     "librarian",
			Title:         "Transferred task",
			ToolsApproved: true,
			Permissions: &session.PermissionsConfig{
				Allow: []string{"exploit_tool"},
				Deny:  []string{"dangerous_tool"},
			},
		},
		SwitchCurrentAgent: true,
	})
	require.NoError(t, err)

	assert.False(t, parent.IsToolsApproved(),
		"a sub-session must not escalate the parent's ToolsApproved gate")
	parentPerms := parent.ClonePermissions()
	require.NotNil(t, parentPerms)
	assert.Empty(t, parentPerms.Allow,
		"child-scope approvals must not leak into the parent's Allow list")
	assert.Equal(t, []string{"dangerous_tool"}, parentPerms.Deny)
}

func TestRunAgent_EndToEndPermissions(t *testing.T) {
	t.Parallel()

	var executed bool
	agentTools := []tools.Tool{{
		Name:       "dangerous_tool",
		Parameters: map[string]any{},
		Handler: func(_ context.Context, _ tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
			executed = true
			return tools.ResultSuccess("executed"), nil
		},
	}}

	workerStream := newStreamBuilder().
		AddToolCallName("call_1", "dangerous_tool").
		AddToolCallArguments("call_1", "{}").
		Build()
	parentProv := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	workerProv := &mockProvider{id: "test/mock-model", stream: workerStream}

	worker := agent.New("worker", "Worker agent",
		agent.WithModel(workerProv),
		agent.WithToolSets(newStubToolSet(nil, agentTools, nil)),
	)
	root := agent.New("root", "Root agent", agent.WithModel(parentProv))
	agent.WithSubAgents(worker)(root)

	tm := team.New(team.WithAgents(root, worker))
	rt, err := NewLocalRuntime(
		t.Context(), tm,
		WithSessionCompaction(false),
		WithModelStore(mockModelStore{}),
	)
	require.NoError(t, err)

	parentPerms := &session.PermissionsConfig{
		Allow: []string{"safe_tool"},
		Deny:  []string{"dangerous_tool"},
	}
	parentSession := session.New(
		session.WithUserMessage("Test"),
		session.WithToolsApproved(true),
		session.WithPermissions(parentPerms),
	)

	result := rt.RunAgent(t.Context(), agenttool.RunParams{
		AgentName:     "worker",
		Task:          "do something",
		ParentSession: parentSession,
	})
	require.Empty(t, result.ErrMsg, "RunAgent should succeed")

	var childSession *session.Session
	for _, item := range parentSession.Messages {
		if item.SubSession != nil {
			childSession = item.SubSession
			break
		}
	}
	require.NotNil(t, childSession, "parent must have a sub-session")
	require.NotNil(t, childSession.Permissions)
	assert.Equal(t, []string{"dangerous_tool"}, childSession.Permissions.Deny,
		"child must inherit the parent's Deny rules")
	require.False(t, executed, "expected dangerous_tool to NOT be executed because it is denied by inherited permissions")
}

func TestTransferTask_PropagatesPermissions(t *testing.T) {
	t.Parallel()

	childStream := newStreamBuilder().AddContent("transferred").AddStopWithUsage(10, 5).Build()
	prov := &mockProvider{id: "test/mock-model", stream: childStream}

	librarian := agent.New("librarian", "Library agent", agent.WithModel(prov))
	root := agent.New("root", "Root agent", agent.WithModel(prov))
	agent.WithSubAgents(librarian)(root)

	tm := team.New(team.WithAgents(root, librarian))
	rt, err := NewLocalRuntime(t.Context(), tm,
		WithSessionCompaction(false),
		WithModelStore(mockModelStore{}),
	)
	require.NoError(t, err)

	parentPerms := &session.PermissionsConfig{
		Allow: []string{"safe_tool"},
		Deny:  []string{"dangerous_tool"},
	}
	sess := session.New(
		session.WithUserMessage("Test"),
		session.WithToolsApproved(true),
		session.WithPermissions(parentPerms),
	)
	evts := make(chan Event, 128)

	toolCall := tools.ToolCall{
		ID:   "call_1",
		Type: "function",
		Function: tools.FunctionCall{
			Name:      "transfer_task",
			Arguments: `{"agent":"librarian","task":"find a book","expected_output":"book title"}`,
		},
	}

	result, err := rt.handleTaskTransfer(t.Context(), sess, toolCall, NewChannelSink(evts))
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.False(t, result.IsError, "transfer to valid sub-agent should succeed")

	var childSession *session.Session
	for _, item := range sess.Messages {
		if item.SubSession != nil {
			childSession = item.SubSession
			break
		}
	}
	require.NotNil(t, childSession, "parent must have a sub-session after transfer_task")

	require.NotNil(t, childSession.Permissions)
	assert.Equal(t, []string{"safe_tool"}, childSession.Permissions.Allow)
	assert.Equal(t, []string{"dangerous_tool"}, childSession.Permissions.Deny)

	assert.True(t, childSession.ToolsApproved,
		"child session must inherit ToolsApproved from parent")

	childSession.Permissions.Allow = append(childSession.Permissions.Allow, "exploit")
	parentClone := sess.ClonePermissions()
	assert.Equal(t, []string{"safe_tool"}, parentClone.Allow,
		"parent permissions must remain isolated from child mutations after transfer_task")
}
