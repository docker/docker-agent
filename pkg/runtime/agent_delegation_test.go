package runtime

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/permissions"
	"github.com/docker/docker-agent/pkg/runtime/toolexec"
	"github.com/docker/docker-agent/pkg/safety"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	agenttool "github.com/docker/docker-agent/pkg/tools/builtin/agent"
	"github.com/docker/docker-agent/pkg/tools/builtin/transfertask"
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

	t.Run("disable structured output", func(t *testing.T) {
		cfg := SubSessionConfig{
			Task:                    "run a fork skill",
			AgentName:               "worker",
			Title:                   "Skill task",
			DisableStructuredOutput: true,
		}

		s := newSubSession(parent, cfg, childAgent)

		assert.True(t, s.DisableStructuredOutput)
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
	assert.False(t, s.DisableStructuredOutput)
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

	decision := toolexec.Decide(s.GetSafetyPolicy(), safety.Label{Class: safety.ClassUnknown}, namedCheckers, "write_file", map[string]any{"path": "foo"})
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

	result, err := rt.handleTaskTransfer(t.Context(), sess, toolCall, NewChannelSink(evts), tools.NopRuntime{})
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

// firstSubSession returns the first sub-session attached to s, or nil.
func firstSubSession(s *session.Session) *session.Session {
	for _, item := range s.MessagesSnapshot() {
		if item.IsSubSession() {
			return item.SubSession
		}
	}
	return nil
}

// transferToolCall builds a transfer_task tool call targeting the given agent.
func transferToolCall(target string) tools.ToolCall {
	return tools.ToolCall{
		ID:   "call_1",
		Type: "function",
		Function: tools.FunctionCall{
			Name:      "transfer_task",
			Arguments: fmt.Sprintf(`{"agent":%q,"task":"do work","expected_output":"result"}`, target),
		},
	}
}

// ancestorNames returns n distinct agent names for crafting delegation lineages.
func ancestorNames(n int) []string {
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("ancestor%d", i)
	}
	return names
}

func TestValidateDelegation(t *testing.T) {
	t.Parallel()

	t.Run("first delegation from a root session", func(t *testing.T) {
		parent := session.New()
		lineage, errMsg := validateDelegation(parent, "root", "worker")
		assert.Empty(t, errMsg)
		assert.Equal(t, []string{"root"}, lineage)
	})

	t.Run("direct cycle", func(t *testing.T) {
		parent := session.New()
		lineage, errMsg := validateDelegation(parent, "a", "a")
		assert.Nil(t, lineage)
		assert.Contains(t, errMsg, "delegation cycle detected: a -> a")
	})

	t.Run("indirect cycle", func(t *testing.T) {
		parent := session.New(session.WithDelegationLineage([]string{"a"}))
		lineage, errMsg := validateDelegation(parent, "b", "a")
		assert.Nil(t, lineage)
		assert.Contains(t, errMsg, "delegation cycle detected: a -> b -> a")
	})

	t.Run("deep indirect cycle names the full attempted path", func(t *testing.T) {
		parent := session.New(session.WithDelegationLineage([]string{"root", "a"}))
		_, errMsg := validateDelegation(parent, "b", "root")
		assert.Contains(t, errMsg, "root -> a -> b -> root")
	})

	t.Run("allows exactly the maximum depth", func(t *testing.T) {
		parent := session.New(session.WithDelegationLineage(ancestorNames(maxDelegationDepth - 1)))
		lineage, errMsg := validateDelegation(parent, "caller", "target")
		assert.Empty(t, errMsg)
		assert.Len(t, lineage, maxDelegationDepth)
	})

	t.Run("rejects one edge past the maximum", func(t *testing.T) {
		parent := session.New(session.WithDelegationLineage(ancestorNames(maxDelegationDepth)))
		lineage, errMsg := validateDelegation(parent, "caller", "target")
		assert.Nil(t, lineage)
		assert.Contains(t, errMsg, "delegation depth limit exceeded")
		assert.Contains(t, errMsg, fmt.Sprintf("at delegation depth %d", maxDelegationDepth))
		assert.Contains(t, errMsg, fmt.Sprintf("reach depth %d", maxDelegationDepth+1))
		assert.Contains(t, errMsg, fmt.Sprintf("maximum of %d", maxDelegationDepth))
	})

	t.Run("sibling delegations never share lineage storage", func(t *testing.T) {
		parent := session.New(session.WithDelegationLineage([]string{"root"}))
		first, errMsg := validateDelegation(parent, "a", "b")
		require.Empty(t, errMsg)
		second, errMsg := validateDelegation(parent, "c", "d")
		require.Empty(t, errMsg)

		first[0] = "mutated"
		assert.Equal(t, []string{"root", "c"}, second)
		assert.Equal(t, []string{"root"}, parent.DelegationLineageSnapshot())
	})
}

func TestNewSubSession_DelegationLineage(t *testing.T) {
	t.Parallel()

	childAgent := agent.New("worker", "")

	t.Run("delegation edge sets and isolates the lineage", func(t *testing.T) {
		parent := session.New(session.WithDelegationLineage([]string{"root"}))
		lineage := []string{"root", "coordinator"}
		s := newSubSession(parent, SubSessionConfig{
			Task:              "work",
			AgentName:         "worker",
			Title:             "Task",
			DelegationLineage: lineage,
		}, childAgent)

		assert.Equal(t, []string{"root", "coordinator"}, s.DelegationLineage)
		lineage[1] = "mutated"
		assert.Equal(t, []string{"root", "coordinator"}, s.DelegationLineage)
	})

	t.Run("nil lineage inherits the parent's unchanged", func(t *testing.T) {
		// Skill sub-sessions leave DelegationLineage nil: they are not
		// delegation edges but must keep ancestry so nested delegations
		// from within a skill are still guarded.
		parent := session.New(session.WithDelegationLineage([]string{"root", "coordinator"}))
		s := newSubSession(parent, SubSessionConfig{
			Task:      "work",
			AgentName: "worker",
			Title:     "Task",
		}, childAgent)

		assert.Equal(t, []string{"root", "coordinator"}, s.DelegationLineage)
	})

	t.Run("root parent yields no lineage", func(t *testing.T) {
		parent := session.New()
		s := newSubSession(parent, SubSessionConfig{
			Task:      "work",
			AgentName: "worker",
			Title:     "Task",
		}, childAgent)

		assert.Empty(t, s.DelegationLineage)
	})
}

func TestTransferTask_RejectsDirectCycle(t *testing.T) {
	t.Parallel()

	prov := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	root := agent.New("root", "Root agent", agent.WithModel(prov))
	// Self-delegation is structurally possible in config; the runtime
	// guard must reject it before a child session is spawned.
	agent.WithSubAgents(root)(root)

	tm := team.New(team.WithAgents(root))
	rt, err := NewLocalRuntime(t.Context(), tm,
		WithSessionCompaction(false),
		WithModelStore(mockModelStore{}),
	)
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("Test"))
	evts := make(chan Event, 128)

	result, err := rt.handleTaskTransfer(t.Context(), sess, transferToolCall("root"), NewChannelSink(evts), tools.NopRuntime{})
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "delegation cycle detected: root -> root")
	assert.Nil(t, firstSubSession(sess), "rejected delegation must not attach a child session")
}

// TestTransferTask_NestedFromPinnedBackgroundSession covers the #3886 nested
// scenario: a background agent (pinned session) calling transfer_task. The
// caller must resolve from the session, so acyclic multi-level delegation
// stays supported while delegating back to an ancestor is rejected.
func TestTransferTask_NestedFromPinnedBackgroundSession(t *testing.T) {
	t.Parallel()

	workerStream := newStreamBuilder().AddContent("worker done").AddStopWithUsage(10, 5).Build()
	helperStream := newStreamBuilder().AddContent("helper done").AddStopWithUsage(10, 5).Build()

	root := agent.New("root", "Root agent", agent.WithModel(&mockProvider{id: "test/mock-model", stream: &mockStream{}}))
	worker := agent.New("worker", "Worker agent", agent.WithModel(&mockProvider{id: "test/mock-model", stream: workerStream}))
	helper := agent.New("helper", "Helper agent", agent.WithModel(&mockProvider{id: "test/mock-model", stream: helperStream}))
	agent.WithSubAgents(worker)(root)
	// helper is only reachable from worker; root also stays in worker's
	// sub-agents so the cycle rejection below is the delegation guard's
	// doing, not the sub-agent list check.
	agent.WithSubAgents(helper, root)(worker)

	tm := team.New(team.WithAgents(root, worker, helper))
	rt, err := NewLocalRuntime(t.Context(), tm,
		WithSessionCompaction(false),
		WithModelStore(mockModelStore{}),
	)
	require.NoError(t, err)

	parent := session.New(session.WithUserMessage("Test"))
	res := rt.RunAgent(t.Context(), agenttool.RunParams{
		AgentName:     "worker",
		Task:          "background work",
		ParentSession: parent,
	})
	require.Empty(t, res.ErrMsg)

	child := firstSubSession(parent)
	require.NotNil(t, child)
	assert.Equal(t, "worker", child.AgentName)
	assert.Equal(t, []string{"root"}, child.DelegationLineage)

	evts := make(chan Event, 128)

	// Delegating back to an ancestor from the pinned child is an indirect cycle.
	result, err := rt.handleTaskTransfer(t.Context(), child, transferToolCall("root"), NewChannelSink(evts), tools.NopRuntime{})
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "root -> worker -> root")
	assert.Nil(t, firstSubSession(child), "rejected delegation must not attach a child session")

	// Acyclic delegation from the same pinned child is allowed. helper is
	// not in root's sub-agents, so success also proves the caller resolved
	// from the pinned session (worker), not the shared current agent (root).
	result, err = rt.handleTaskTransfer(t.Context(), child, transferToolCall("helper"), NewChannelSink(evts), tools.NopRuntime{})
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.False(t, result.IsError, "acyclic multi-level delegation must stay supported: %s", result.Output)

	grandchild := firstSubSession(child)
	require.NotNil(t, grandchild)
	assert.Equal(t, "helper", grandchild.AgentName,
		"nested transfer children from a pinned parent are pinned to the target")
	assert.Equal(t, []string{"root", "worker"}, grandchild.DelegationLineage)
	assert.Equal(t, []string{"root"}, child.DelegationLineage,
		"sibling delegations must not mutate the parent's lineage")
}

// probeTool returns a single-tool list whose handler invokes record at
// execution time. Tests use it to observe the runtime's shared current
// agent from inside a running sub-session.
func probeTool(record func()) []tools.Tool {
	return []tools.Tool{{
		Name:       "probe",
		Parameters: map[string]any{},
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			record()
			return tools.ResultSuccess("probed"), nil
		},
	}}
}

// probeAgent builds an agent that makes one "probe" tool call (invoking
// record mid-run) and then replies with content.
func probeAgent(name, content string, record func()) *agent.Agent {
	prov := &queueProvider{id: "test/mock-model", streams: []chat.MessageStream{
		newStreamBuilder().AddToolCallWithStop("call_probe", "probe", "{}").Build(),
		newStreamBuilder().AddContent(content).AddStopWithUsage(10, 5).Build(),
	}}
	return agent.New(name, name+" agent",
		agent.WithModel(prov),
		agent.WithToolSets(newStubToolSet(nil, probeTool(record), nil)),
	)
}

// collectTransferEvents closes evts and returns the AgentSwitching events and
// the SubSessionCompleted event it carried.
func collectTransferEvents(evts chan Event) (switches []*AgentSwitchingEvent, completed *SubSessionCompletedEvent) {
	close(evts)
	for ev := range evts {
		switch e := ev.(type) {
		case *AgentSwitchingEvent:
			switches = append(switches, e)
		case *SubSessionCompletedEvent:
			completed = e
		}
	}
	return switches, completed
}

// TestTransferTask_PinnedParentDoesNotMutateSharedCurrentAgent reproduces the
// #3886 nested misattribution: transfer_task from a pinned background session
// used to enter the shared switch path, so runForwarding re-resolved the
// caller to the foreground agent (root), swapCurrentAgent mutated the shared
// current agent mid-flight, and SubSessionCompleted plus subagent_stop were
// attributed to root. The child must instead be pinned to the target, run as
// the target, and be attributed to the actual pinned caller (worker).
func TestTransferTask_PinnedParentDoesNotMutateSharedCurrentAgent(t *testing.T) {
	t.Parallel()

	var rt *LocalRuntime
	var mu sync.Mutex
	var observed []string
	record := func() {
		mu.Lock()
		defer mu.Unlock()
		observed = append(observed, rt.CurrentAgent().Name())
	}

	workerStream := newStreamBuilder().AddContent("worker done").AddStopWithUsage(10, 5).Build()
	root := agent.New("root", "Root agent", agent.WithModel(&mockProvider{id: "test/mock-model", stream: &mockStream{}}))
	worker := agent.New("worker", "Worker agent",
		agent.WithModel(&mockProvider{id: "test/mock-model", stream: workerStream}),
		// The recording subagent_stop hook lives on worker: it only fires
		// when the runtime attributes the nested transfer to the pinned
		// caller rather than the shared current agent (root).
		agent.WithHooks(&hooks.Config{
			SubagentStop: []hooks.Hook{{Type: hooks.HookTypeBuiltin, Command: "test_record_subagent_stop"}},
		}),
	)
	helper := probeAgent("helper", "helper done", record)
	agent.WithSubAgents(worker)(root)
	agent.WithSubAgents(helper)(worker)

	tm := team.New(team.WithAgents(root, worker, helper))
	var err error
	rt, err = NewLocalRuntime(t.Context(), tm,
		WithSessionCompaction(false),
		WithModelStore(mockModelStore{}),
	)
	require.NoError(t, err)

	rb := &recordingBuiltin{}
	require.NoError(t, rt.hooksRegistry.RegisterBuiltin("test_record_subagent_stop", rb.hook))
	rt.buildHooksExecutors()

	parent := session.New(session.WithUserMessage("Test"), session.WithToolsApproved(true))
	res := rt.RunAgent(t.Context(), agenttool.RunParams{
		AgentName:     "worker",
		Task:          "background work",
		ParentSession: parent,
	})
	require.Empty(t, res.ErrMsg)

	child := firstSubSession(parent)
	require.NotNil(t, child)
	require.Equal(t, "worker", child.AgentName, "background child session must be pinned")

	evts := make(chan Event, 128)
	result, err := rt.handleTaskTransfer(t.Context(), child, transferToolCall("helper"), NewChannelSink(evts), tools.NopRuntime{})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.IsError, "nested transfer must succeed: %s", result.Output)
	assert.Equal(t, "helper done", result.Output, "the child must run as the target agent")

	mu.Lock()
	assert.Equal(t, []string{"root"}, observed,
		"shared current agent must stay untouched while the pinned child's transfer runs")
	mu.Unlock()
	assert.Equal(t, "root", rt.CurrentAgent().Name())

	grandchild := firstSubSession(child)
	require.NotNil(t, grandchild)
	assert.Equal(t, "helper", grandchild.AgentName, "grandchild must be pinned to the target agent")

	switches, completed := collectTransferEvents(evts)
	assert.Empty(t, switches, "a pinned-parent transfer must not emit AgentSwitching events")
	require.NotNil(t, completed, "SubSessionCompleted must still be emitted")
	assert.Equal(t, "worker", completed.GetAgentName(),
		"SubSessionCompleted must be attributed to the pinned caller")

	stops := rb.snapshot()
	require.Len(t, stops, 1, "worker's subagent_stop hook must fire exactly once")
	assert.Equal(t, "helper", stops[0].AgentName)
	assert.Equal(t, child.ID, stops[0].ParentSessionID)
	assert.Equal(t, grandchild.ID, stops[0].SessionID)
	assert.Equal(t, "helper done", stops[0].StopResponse)
}

// TestTransferTask_ForegroundSwitchesAndRestoresCurrentAgent pins the
// sequential semantics for ordinary foreground transfers: the shared current
// agent is switched to the target while the child runs (the child session
// stays unpinned and resolves through it), then restored, with entry and
// return AgentSwitching events around the run.
func TestTransferTask_ForegroundSwitchesAndRestoresCurrentAgent(t *testing.T) {
	t.Parallel()

	var rt *LocalRuntime
	var mu sync.Mutex
	var observed []string
	record := func() {
		mu.Lock()
		defer mu.Unlock()
		observed = append(observed, rt.CurrentAgent().Name())
	}

	librarian := probeAgent("librarian", "found it", record)
	root := agent.New("root", "Root agent", agent.WithModel(&mockProvider{id: "test/mock-model", stream: &mockStream{}}))
	agent.WithSubAgents(librarian)(root)

	tm := team.New(team.WithAgents(root, librarian))
	var err error
	rt, err = NewLocalRuntime(t.Context(), tm,
		WithSessionCompaction(false),
		WithModelStore(mockModelStore{}),
	)
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("Test"), session.WithToolsApproved(true))
	evts := make(chan Event, 128)

	result, err := rt.handleTaskTransfer(t.Context(), sess, transferToolCall("librarian"), NewChannelSink(evts), tools.NopRuntime{})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.IsError, "transfer must succeed: %s", result.Output)
	assert.Equal(t, "found it", result.Output)

	mu.Lock()
	assert.Equal(t, []string{"librarian"}, observed,
		"the shared current agent must point at the target while the child runs")
	mu.Unlock()
	assert.Equal(t, "root", rt.CurrentAgent().Name(), "the shared current agent must be restored afterwards")

	child := firstSubSession(sess)
	require.NotNil(t, child)
	assert.Empty(t, child.AgentName, "foreground transfer children stay unpinned")

	switches, completed := collectTransferEvents(evts)
	require.Len(t, switches, 2, "entry and return AgentSwitching events must be emitted")
	assert.True(t, switches[0].Switching)
	assert.Equal(t, "root", switches[0].FromAgent)
	assert.Equal(t, "librarian", switches[0].ToAgent)
	assert.False(t, switches[1].Switching)
	assert.Equal(t, "librarian", switches[1].FromAgent)
	assert.Equal(t, "root", switches[1].ToAgent)
	require.NotNil(t, completed)
	assert.Equal(t, "root", completed.GetAgentName())
}

// TestTransferTask_ConcurrentPinnedNestedTransfersStayIsolated runs two
// nested transfers from two pinned background sessions concurrently. Each
// child must execute as its own target via session pinning, with the shared
// current agent never leaving root — the corruption mode before the fix,
// where each transfer swapped the shared field and could misroute the other
// (and any foreground) session's agent resolution.
func TestTransferTask_ConcurrentPinnedNestedTransfersStayIsolated(t *testing.T) {
	t.Parallel()

	var rt *LocalRuntime
	var mu sync.Mutex
	var observed []string
	record := func() {
		mu.Lock()
		defer mu.Unlock()
		observed = append(observed, rt.CurrentAgent().Name())
	}

	root := agent.New("root", "Root agent", agent.WithModel(&mockProvider{id: "test/mock-model", stream: &mockStream{}}))
	newWorker := func(name string, helper *agent.Agent) *agent.Agent {
		stream := newStreamBuilder().AddContent(name+" done").AddStopWithUsage(10, 5).Build()
		w := agent.New(name, "Worker agent", agent.WithModel(&mockProvider{id: "test/mock-model", stream: stream}))
		agent.WithSubAgents(helper)(w)
		return w
	}
	helperA := probeAgent("helperA", "helperA done", record)
	helperB := probeAgent("helperB", "helperB done", record)
	workerA := newWorker("workerA", helperA)
	workerB := newWorker("workerB", helperB)
	agent.WithSubAgents(workerA, workerB)(root)

	tm := team.New(team.WithAgents(root, workerA, workerB, helperA, helperB))
	var err error
	rt, err = NewLocalRuntime(t.Context(), tm,
		WithSessionCompaction(false),
		WithModelStore(mockModelStore{}),
	)
	require.NoError(t, err)

	spawn := func(workerName string) *session.Session {
		p := session.New(session.WithUserMessage("Test"), session.WithToolsApproved(true))
		res := rt.RunAgent(t.Context(), agenttool.RunParams{
			AgentName:     workerName,
			Task:          "background work",
			ParentSession: p,
		})
		require.Empty(t, res.ErrMsg)
		child := firstSubSession(p)
		require.NotNil(t, child)
		return child
	}
	childA := spawn("workerA")
	childB := spawn("workerB")

	type transferOutcome struct {
		result *tools.ToolCallResult
		err    error
		evts   chan Event
	}
	run := func(child *session.Session, target string) chan transferOutcome {
		out := make(chan transferOutcome, 1)
		go func() {
			evts := make(chan Event, 128)
			result, err := rt.handleTaskTransfer(t.Context(), child, transferToolCall(target), NewChannelSink(evts), tools.NopRuntime{})
			out <- transferOutcome{result: result, err: err, evts: evts}
		}()
		return out
	}
	outA := run(childA, "helperA")
	outB := run(childB, "helperB")
	a := <-outA
	b := <-outB

	require.NoError(t, a.err)
	require.NotNil(t, a.result)
	require.False(t, a.result.IsError, "workerA's transfer must succeed: %s", a.result.Output)
	assert.Equal(t, "helperA done", a.result.Output)
	require.NoError(t, b.err)
	require.NotNil(t, b.result)
	require.False(t, b.result.IsError, "workerB's transfer must succeed: %s", b.result.Output)
	assert.Equal(t, "helperB done", b.result.Output)

	mu.Lock()
	assert.Equal(t, []string{"root", "root"}, observed,
		"neither concurrent pinned transfer may mutate the shared current agent")
	mu.Unlock()
	assert.Equal(t, "root", rt.CurrentAgent().Name())

	grandA := firstSubSession(childA)
	require.NotNil(t, grandA)
	assert.Equal(t, "helperA", grandA.AgentName)
	grandB := firstSubSession(childB)
	require.NotNil(t, grandB)
	assert.Equal(t, "helperB", grandB.AgentName)

	switchesA, completedA := collectTransferEvents(a.evts)
	assert.Empty(t, switchesA, "pinned transfers must not emit AgentSwitching events")
	require.NotNil(t, completedA)
	assert.Equal(t, "workerA", completedA.GetAgentName())
	switchesB, completedB := collectTransferEvents(b.evts)
	assert.Empty(t, switchesB, "pinned transfers must not emit AgentSwitching events")
	require.NotNil(t, completedB)
	assert.Equal(t, "workerB", completedB.GetAgentName())
}

// callerRuntime is a [tools.Runtime] reporting a fixed caller agent, standing in
// for the per-call handle the dispatcher builds from its pre-fan-out snapshot.
type callerRuntime struct {
	tools.NopRuntime

	caller *agent.Agent
}

func (r callerRuntime) CallerAgent() *agent.Agent { return r.caller }

// TestTransferTask_UsesBatchCallerSnapshotNotSharedCurrentAgent pins the exact
// failure reported in #4156: a sibling transfer_task from the same parallel
// batch has already swapped the shared current agent to its own target, so
// re-resolving the caller from it identified the target as its own caller and
// rejected the transfer with "No agents are configured in this list".
func TestTransferTask_UsesBatchCallerSnapshotNotSharedCurrentAgent(t *testing.T) {
	t.Parallel()

	drafter := agent.New("drafter", "Drafter agent", agent.WithModel(&mockProvider{
		id:     "test/mock-model",
		stream: newStreamBuilder().AddContent("drafter done").AddStopWithUsage(10, 5).Build(),
	}))
	root := agent.New("root", "Root agent",
		agent.WithModel(&mockProvider{id: "test/mock-model", stream: &mockStream{}}),
		agent.WithSubAgents(drafter),
	)

	tm := team.New(team.WithAgents(root, drafter))
	rt, err := NewLocalRuntime(t.Context(), tm,
		WithSessionCompaction(false),
		WithModelStore(mockModelStore{}),
	)
	require.NoError(t, err)

	// Stand in for the sibling that already claimed the shared current agent.
	rt.setCurrentAgent("drafter")

	sess := session.New(session.WithUserMessage("Test"), session.WithToolsApproved(true))
	result, err := rt.handleTaskTransfer(t.Context(), sess, transferToolCall("drafter"),
		NewChannelSink(make(chan Event, 128)), callerRuntime{caller: root})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.IsError,
		"the caller must come from the batch snapshot (root), not the swapped current agent: %s", result.Output)
	assert.Equal(t, "drafter done", result.Output)
}

// TestTransferTask_ConcurrentForegroundTransfersStayIsolated reproduces #4156:
// the model issues three transfer_task calls in a single response and the
// dispatcher runs them in parallel. Each goroutine used to re-read the shared
// current agent that a sibling's swapCurrentAgent had already mutated, so the
// later calls validated the target against the wrong caller and failed with
// "target agent not in sub-agents list", while a child that did start could run
// its turns as another sibling's agent. Every transfer must instead run as its
// own target and leave the shared current agent back at root.
func TestTransferTask_ConcurrentForegroundTransfersStayIsolated(t *testing.T) {
	t.Parallel()

	targets := []string{"drafter", "reviewer", "tester"}
	subAgents := make([]*agent.Agent, 0, len(targets))
	for _, name := range targets {
		prov := &mockProvider{id: "test/mock-model", stream: newStreamBuilder().
			AddContent(name+" done").AddStopWithUsage(10, 5).Build()}
		subAgents = append(subAgents, agent.New(name, name+" agent", agent.WithModel(prov)))
	}

	// One assistant response carrying all three calls — the parallel tool use
	// that triggers the race.
	batch := newStreamBuilder()
	for i, name := range targets {
		id := transferCallID(i)
		batch.AddToolCallName(id, transfertask.ToolNameTransferTask).
			AddToolCallArguments(id, fmt.Sprintf(`{"agent":%q,"task":"chunk %d","expected_output":"result"}`, name, i))
	}
	rootProv := &queueProvider{id: "test/mock-model", streams: []chat.MessageStream{
		batch.AddToolCallStopWithUsage(10, 5).Build(),
		newStreamBuilder().AddContent("all delegated").AddStopWithUsage(10, 5).Build(),
	}}

	root := agent.New("root", "Root agent",
		agent.WithModel(rootProv),
		agent.WithSubAgents(subAgents...),
		agent.WithToolSets(transfertask.New()),
	)

	tm := team.New(team.WithAgents(append([]*agent.Agent{root}, subAgents...)...))
	rt, err := NewLocalRuntime(t.Context(), tm,
		WithSessionCompaction(false),
		WithModelStore(mockModelStore{}),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close() })

	sess := session.New(session.WithUserMessage("split the diff"), session.WithToolsApproved(true))
	_, err = rt.Run(t.Context(), sess)
	require.NoError(t, err)

	for i, name := range targets {
		out := toolResultContent(t, sess, transferCallID(i))
		assert.Equal(t, name+" done", out,
			"transfer to %s must run as %s, not as a sibling's target", name, name)
	}
	assert.Equal(t, "root", rt.CurrentAgent().Name(),
		"the shared current agent must be back at root once the batch drains")
}

// transferCallID names the nth transfer_task call of a parallel batch.
func transferCallID(i int) string { return fmt.Sprintf("call_transfer_%d", i) }

func TestTransferTask_DepthBoundary(t *testing.T) {
	t.Parallel()

	childStream := newStreamBuilder().AddContent("done").AddStopWithUsage(10, 5).Build()
	librarian := agent.New("librarian", "Library agent", agent.WithModel(&mockProvider{id: "test/mock-model", stream: childStream}))
	root := agent.New("root", "Root agent", agent.WithModel(&mockProvider{id: "test/mock-model", stream: &mockStream{}}))
	agent.WithSubAgents(librarian)(root)

	tm := team.New(team.WithAgents(root, librarian))
	rt, err := NewLocalRuntime(t.Context(), tm,
		WithSessionCompaction(false),
		WithModelStore(mockModelStore{}),
	)
	require.NoError(t, err)

	t.Run("allows exactly the maximum depth", func(t *testing.T) {
		sess := session.New(
			session.WithUserMessage("Test"),
			session.WithDelegationLineage(ancestorNames(maxDelegationDepth-1)),
		)
		evts := make(chan Event, 128)

		result, err := rt.handleTaskTransfer(t.Context(), sess, transferToolCall("librarian"), NewChannelSink(evts), tools.NopRuntime{})
		require.NoError(t, err)
		require.NotNil(t, result)
		assert.False(t, result.IsError, "delegation at the maximum depth must be allowed: %s", result.Output)

		child := firstSubSession(sess)
		require.NotNil(t, child)
		assert.Len(t, child.DelegationLineage, maxDelegationDepth)
	})

	t.Run("rejects one edge past the maximum", func(t *testing.T) {
		sess := session.New(
			session.WithUserMessage("Test"),
			session.WithDelegationLineage(ancestorNames(maxDelegationDepth)),
		)
		evts := make(chan Event, 128)

		result, err := rt.handleTaskTransfer(t.Context(), sess, transferToolCall("librarian"), NewChannelSink(evts), tools.NopRuntime{})
		require.NoError(t, err)
		require.NotNil(t, result)
		assert.True(t, result.IsError)
		assert.Contains(t, result.Output, "delegation depth limit exceeded")
		assert.Contains(t, result.Output, fmt.Sprintf("reach depth %d", maxDelegationDepth+1))
		assert.Contains(t, result.Output, fmt.Sprintf("maximum of %d", maxDelegationDepth))
		assert.Nil(t, firstSubSession(sess), "rejected delegation must not attach a child session")
	})
}

func TestRunAgent_RejectsDelegationCycle(t *testing.T) {
	t.Parallel()

	workerStream := newStreamBuilder().AddContent("done").AddStopWithUsage(10, 5).Build()
	worker := agent.New("worker", "Worker agent", agent.WithModel(&mockProvider{id: "test/mock-model", stream: workerStream}))
	root := agent.New("root", "Root agent", agent.WithModel(&mockProvider{id: "test/mock-model", stream: &mockStream{}}))
	agent.WithSubAgents(worker)(root)
	agent.WithSubAgents(root)(worker)

	tm := team.New(team.WithAgents(root, worker))
	rt, err := NewLocalRuntime(t.Context(), tm,
		WithSessionCompaction(false),
		WithModelStore(mockModelStore{}),
	)
	require.NoError(t, err)

	parent := session.New(session.WithUserMessage("Test"))
	res := rt.RunAgent(t.Context(), agenttool.RunParams{
		AgentName:     "worker",
		Task:          "background work",
		ParentSession: parent,
	})
	require.Empty(t, res.ErrMsg)

	child := firstSubSession(parent)
	require.NotNil(t, child)
	assert.Equal(t, []string{"root"}, child.DelegationLineage)

	// Delegating back to an ancestor from the background child must fail
	// before any child session is spawned.
	res = rt.RunAgent(t.Context(), agenttool.RunParams{
		AgentName:     "root",
		Task:          "loop back",
		ParentSession: child,
	})
	assert.Contains(t, res.ErrMsg, "delegation cycle detected")
	assert.Contains(t, res.ErrMsg, "root -> worker -> root")
	assert.Nil(t, firstSubSession(child), "rejected delegation must not attach a child session")
}

// TestRunAgent_NestedAcyclicDelegation verifies that a background agent can
// itself dispatch a background agent, and that the caller resolves from the
// pinned parent session: with the shared current agent (root) the grandchild
// lineage would be [root root] instead of [root worker].
func TestRunAgent_NestedAcyclicDelegation(t *testing.T) {
	t.Parallel()

	workerStream := newStreamBuilder().AddContent("worker done").AddStopWithUsage(10, 5).Build()
	helperStream := newStreamBuilder().AddContent("helper done").AddStopWithUsage(10, 5).Build()

	root := agent.New("root", "Root agent", agent.WithModel(&mockProvider{id: "test/mock-model", stream: &mockStream{}}))
	worker := agent.New("worker", "Worker agent", agent.WithModel(&mockProvider{id: "test/mock-model", stream: workerStream}))
	helper := agent.New("helper", "Helper agent", agent.WithModel(&mockProvider{id: "test/mock-model", stream: helperStream}))
	agent.WithSubAgents(worker)(root)
	agent.WithSubAgents(helper)(worker)

	tm := team.New(team.WithAgents(root, worker, helper))
	rt, err := NewLocalRuntime(t.Context(), tm,
		WithSessionCompaction(false),
		WithModelStore(mockModelStore{}),
	)
	require.NoError(t, err)

	parent := session.New(session.WithUserMessage("Test"))
	res := rt.RunAgent(t.Context(), agenttool.RunParams{
		AgentName:     "worker",
		Task:          "background work",
		ParentSession: parent,
	})
	require.Empty(t, res.ErrMsg)

	child := firstSubSession(parent)
	require.NotNil(t, child)

	res = rt.RunAgent(t.Context(), agenttool.RunParams{
		AgentName:     "helper",
		Task:          "nested background work",
		ParentSession: child,
	})
	require.Empty(t, res.ErrMsg, "acyclic nested background delegation must stay supported")

	grandchild := firstSubSession(child)
	require.NotNil(t, grandchild)
	assert.Equal(t, "helper", grandchild.AgentName)
	assert.Equal(t, []string{"root", "worker"}, grandchild.DelegationLineage,
		"caller must resolve from the pinned parent session, not the shared current agent")
	assert.Equal(t, []string{"root"}, child.DelegationLineage)
}

// TestRunAgent_NestedBackgroundSubagentStopFiresOnPinnedCaller covers hook
// attribution for nested background → background delegation: when a pinned
// worker session dispatches run_background_agent to helper, the subagent_stop
// hook belongs to the pinned caller (worker), not to whatever the shared
// current agent points at (root). Before the fix, runCollecting's deferred
// hook used r.CurrentAgent(), so root's hook received the helper completion
// and worker's never fired.
//
// In production the nested dispatch reaches the Runner entry (RunAgent) on a
// detached goroutine owned by the run_background_agent handler; calling
// rt.RunAgent synchronously here drives the exact same entry point and hook
// path without the goroutine indirection.
func TestRunAgent_NestedBackgroundSubagentStopFiresOnPinnedCaller(t *testing.T) {
	t.Parallel()

	workerStream := newStreamBuilder().AddContent("worker done").AddStopWithUsage(10, 5).Build()
	helperStream := newStreamBuilder().AddContent("helper done").AddStopWithUsage(10, 5).Build()

	root := agent.New("root", "Root agent",
		agent.WithModel(&mockProvider{id: "test/mock-model", stream: &mockStream{}}),
		agent.WithHooks(&hooks.Config{
			SubagentStop: []hooks.Hook{{Type: hooks.HookTypeBuiltin, Command: "test_record_subagent_stop_root"}},
		}),
	)
	worker := agent.New("worker", "Worker agent",
		agent.WithModel(&mockProvider{id: "test/mock-model", stream: workerStream}),
		agent.WithHooks(&hooks.Config{
			SubagentStop: []hooks.Hook{{Type: hooks.HookTypeBuiltin, Command: "test_record_subagent_stop_worker"}},
		}),
	)
	helper := agent.New("helper", "Helper agent", agent.WithModel(&mockProvider{id: "test/mock-model", stream: helperStream}))
	agent.WithSubAgents(worker)(root)
	agent.WithSubAgents(helper)(worker)

	tm := team.New(team.WithAgents(root, worker, helper))
	rt, err := NewLocalRuntime(t.Context(), tm,
		WithSessionCompaction(false),
		WithModelStore(mockModelStore{}),
	)
	require.NoError(t, err)

	rbRoot := &recordingBuiltin{}
	rbWorker := &recordingBuiltin{}
	require.NoError(t, rt.hooksRegistry.RegisterBuiltin("test_record_subagent_stop_root", rbRoot.hook))
	require.NoError(t, rt.hooksRegistry.RegisterBuiltin("test_record_subagent_stop_worker", rbWorker.hook))
	rt.buildHooksExecutors()

	parent := session.New(session.WithUserMessage("Test"))
	res := rt.RunAgent(t.Context(), agenttool.RunParams{
		AgentName:     "worker",
		Task:          "background work",
		ParentSession: parent,
	})
	require.Empty(t, res.ErrMsg)

	child := firstSubSession(parent)
	require.NotNil(t, child)
	require.Equal(t, "worker", child.AgentName, "background child session must be pinned")

	res = rt.RunAgent(t.Context(), agenttool.RunParams{
		AgentName:     "helper",
		Task:          "nested background work",
		ParentSession: child,
	})
	require.Empty(t, res.ErrMsg)

	grandchild := firstSubSession(child)
	require.NotNil(t, grandchild)

	workerStops := rbWorker.snapshot()
	require.Len(t, workerStops, 1, "worker's subagent_stop hook must fire exactly once for the nested completion")
	assert.Equal(t, "helper", workerStops[0].AgentName)
	assert.Equal(t, child.ID, workerStops[0].ParentSessionID)
	assert.Equal(t, grandchild.ID, workerStops[0].SessionID)
	assert.Equal(t, "helper done", workerStops[0].StopResponse)

	// Root's hook sees only its own direct child (the first, unpinned
	// dispatch resolves to the shared current agent, root); the nested
	// helper completion must not reach it.
	rootStops := rbRoot.snapshot()
	require.Len(t, rootStops, 1, "root's subagent_stop hook must not receive the nested helper completion")
	assert.Equal(t, "worker", rootStops[0].AgentName)
	assert.Equal(t, parent.ID, rootStops[0].ParentSessionID)
	assert.Equal(t, child.ID, rootStops[0].SessionID)
	assert.Equal(t, "worker done", rootStops[0].StopResponse)
}

func TestRunAgent_DepthBoundary(t *testing.T) {
	t.Parallel()

	workerStream := newStreamBuilder().AddContent("done").AddStopWithUsage(10, 5).Build()
	worker := agent.New("worker", "Worker agent", agent.WithModel(&mockProvider{id: "test/mock-model", stream: workerStream}))
	root := agent.New("root", "Root agent", agent.WithModel(&mockProvider{id: "test/mock-model", stream: &mockStream{}}))
	agent.WithSubAgents(worker)(root)

	tm := team.New(team.WithAgents(root, worker))
	rt, err := NewLocalRuntime(t.Context(), tm,
		WithSessionCompaction(false),
		WithModelStore(mockModelStore{}),
	)
	require.NoError(t, err)

	t.Run("allows exactly the maximum depth", func(t *testing.T) {
		parent := session.New(
			session.WithUserMessage("Test"),
			session.WithDelegationLineage(ancestorNames(maxDelegationDepth-1)),
		)
		res := rt.RunAgent(t.Context(), agenttool.RunParams{
			AgentName:     "worker",
			Task:          "deep work",
			ParentSession: parent,
		})
		require.Empty(t, res.ErrMsg, "delegation at the maximum depth must be allowed")

		child := firstSubSession(parent)
		require.NotNil(t, child)
		assert.Len(t, child.DelegationLineage, maxDelegationDepth)
	})

	t.Run("rejects one edge past the maximum", func(t *testing.T) {
		parent := session.New(
			session.WithUserMessage("Test"),
			session.WithDelegationLineage(ancestorNames(maxDelegationDepth)),
		)
		res := rt.RunAgent(t.Context(), agenttool.RunParams{
			AgentName:     "worker",
			Task:          "too deep",
			ParentSession: parent,
		})
		assert.Contains(t, res.ErrMsg, "delegation depth limit exceeded")
		assert.Contains(t, res.ErrMsg, fmt.Sprintf("reach depth %d", maxDelegationDepth+1))
		assert.Contains(t, res.ErrMsg, fmt.Sprintf("maximum of %d", maxDelegationDepth))
		assert.Nil(t, firstSubSession(parent), "rejected delegation must not attach a child session")
	})
}

// enqueue appends scripted streams to the provider after construction. Used
// when a later turn's arguments are only known mid-test (e.g. a dynamic
// background task ID parsed from an earlier tool result). Safe to call
// concurrently with CreateChatCompletionStream.
func (p *queueProvider) enqueue(streams ...chat.MessageStream) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.streams = append(p.streams, streams...)
}

// toolResultContent returns the recorded tool result for the given tool call
// ID from the session's own messages, failing the test when absent.
func toolResultContent(t *testing.T, sess *session.Session, toolCallID string) string {
	t.Helper()
	for _, m := range sess.OwnMessages() {
		if m.Message.Role == chat.MessageRoleTool && m.Message.ToolCallID == toolCallID {
			return m.Message.Content
		}
	}
	t.Fatalf("no tool result recorded for tool call %q", toolCallID)
	return ""
}

// parseBackgroundTaskID extracts the dynamic task ID from a
// run_background_agent tool result.
func parseBackgroundTaskID(t *testing.T, dispatchOutput string) string {
	t.Helper()
	const marker = "Background agent task started with ID: "
	_, rest, ok := strings.Cut(dispatchOutput, marker)
	require.True(t, ok, "dispatch result must announce the task ID, got: %s", dispatchOutput)
	id, _, _ := strings.Cut(rest, "\n")
	id = strings.TrimSpace(id)
	require.NotEmpty(t, id, "task ID must not be empty")
	return id
}

// TestRunStream_NestedBackgroundAgents_EndToEnd is the true asynchronous
// end-to-end regression test for #3904/#3886. Unlike the tests above, it
// never calls RunAgent or handleTaskTransfer directly: root is driven
// through rt.Run, so its model's run_background_agent tool call dispatches
// through r.toolMap into the real agenttool.Handler.HandleRun, which returns
// a task ID immediately and runs the worker on the real detached goroutine.
// Inside that detached RunStream the worker's model again calls
// run_background_agent (nested background → background), and root later
// inspects completion through the real list/view handlers via further model
// tool calls.
//
// This runtime integration test qualifies as end-to-end because it executes
// the entire production async path — model stream → tool dispatch →
// HandleRun → detached goroutine → nested HandleRun → task bookkeeping →
// polling handlers — in one process with no external network; only the model
// providers are scripted.
//
// Synchronisation is deterministic: subagent_stop hook channels gate the
// worker's final turn and the test's own progress, and task completion is
// awaited by polling the real registered list_background_agents handler
// under a bounded deadline. No arbitrary sleeps.
func TestRunStream_NestedBackgroundAgents_EndToEnd(t *testing.T) {
	t.Parallel()

	// helperDrained closes when worker's subagent_stop hook fires (the
	// nested helper sub-session fully drained); it gates worker's final
	// turn. workerDrained closes when root's subagent_stop hook fires (the
	// worker sub-session fully drained); it gates the test's progress.
	helperDrained := make(chan struct{})
	workerDrained := make(chan struct{})

	rootProv := &queueProvider{id: "test/mock-model", streams: []chat.MessageStream{
		newStreamBuilder().
			AddToolCallName("call_run_worker", agenttool.ToolNameRunBackgroundAgent).
			AddToolCallArguments("call_run_worker", `{"agent":"worker","task":"do the background work"}`).
			AddToolCallStopWithUsage(10, 5).
			Build(),
		newStreamBuilder().AddContent("dispatched worker").AddStopWithUsage(10, 5).Build(),
	}}
	// The release gate makes worker's final turn wait until the nested
	// helper task has drained — a deterministic stand-in for the "poll
	// until done" turns a real model would issue.
	workerProv := &stepProvider{id: "test/mock-model", steps: []providerStep{
		{stream: newStreamBuilder().
			AddToolCallName("call_run_helper", agenttool.ToolNameRunBackgroundAgent).
			AddToolCallArguments("call_run_helper", `{"agent":"helper","task":"do the nested work"}`).
			AddToolCallStopWithUsage(10, 5).
			Build()},
		{release: helperDrained, stream: newStreamBuilder().AddContent("worker done").AddStopWithUsage(10, 5).Build()},
	}}
	helperProv := &mockProvider{
		id:     "test/mock-model",
		stream: newStreamBuilder().AddContent("nested helper done").AddStopWithUsage(10, 5).Build(),
	}

	helper := agent.New("helper", "Helper agent", agent.WithModel(helperProv))
	// helper is reachable only from worker: the nested dispatch can only
	// succeed when HandleRun validates the target against the pinned
	// caller (worker), not the shared current agent (root) — #3886.
	worker := agent.New("worker", "Worker agent",
		agent.WithModel(workerProv),
		agent.WithSubAgents(helper),
		agent.WithToolSets(agenttool.New()),
		agent.WithHooks(&hooks.Config{
			SubagentStop: []hooks.Hook{{Type: hooks.HookTypeBuiltin, Command: "test_signal_worker_subagent_stop"}},
		}),
	)
	root := agent.New("root", "Root agent",
		agent.WithModel(rootProv),
		agent.WithSubAgents(worker),
		agent.WithToolSets(agenttool.New()),
		agent.WithHooks(&hooks.Config{
			SubagentStop: []hooks.Hook{{Type: hooks.HookTypeBuiltin, Command: "test_signal_root_subagent_stop"}},
		}),
	)

	tm := team.New(team.WithAgents(root, worker, helper))
	rt, err := NewLocalRuntime(t.Context(), tm,
		WithSessionCompaction(false),
		WithModelStore(mockModelStore{}),
	)
	require.NoError(t, err)
	// StopAll cancels and waits for detached task goroutines, so they
	// cannot leak past the test even on a failure path.
	t.Cleanup(func() { _ = rt.Close() })

	rbRoot := &recordingBuiltin{}
	rbWorker := &recordingBuiltin{}
	var rootOnce, workerOnce sync.Once
	require.NoError(t, rt.hooksRegistry.RegisterBuiltin("test_signal_root_subagent_stop",
		func(ctx context.Context, in *hooks.Input, args []string) (*hooks.Output, error) {
			out, err := rbRoot.hook(ctx, in, args)
			rootOnce.Do(func() { close(workerDrained) })
			return out, err
		}))
	require.NoError(t, rt.hooksRegistry.RegisterBuiltin("test_signal_worker_subagent_stop",
		func(ctx context.Context, in *hooks.Input, args []string) (*hooks.Output, error) {
			out, err := rbWorker.hook(ctx, in, args)
			workerOnce.Do(func() { close(helperDrained) })
			return out, err
		}))
	rt.buildHooksExecutors()

	sess := session.New(session.WithUserMessage("dispatch the worker"), session.WithToolsApproved(true))

	// Turn 1: root's model dispatches the worker. HandleRun must return a
	// task ID immediately while the worker runs detached.
	_, err = rt.Run(t.Context(), sess)
	require.NoError(t, err)

	dispatchOut := toolResultContent(t, sess, "call_run_worker")
	require.Contains(t, dispatchOut, "Background agent task started with ID: ",
		"run_background_agent must return a task ID immediately (async dispatch)")
	taskID := parseBackgroundTaskID(t, dispatchOut)

	// The whole nested chain runs on detached goroutines that never touch
	// the runtime's shared current agent.
	waitClosed(t, workerDrained, "worker background task to drain")
	assert.Equal(t, "root", rt.CurrentAgent().Name(),
		"shared current agent must remain root while background tasks run")

	// The subagent_stop hooks fire just before HandleRun's goroutines mark
	// their tasks completed, so await both terminal statuses through the
	// real registered list handler (bounded, no fixed sleep budget).
	listHandler := rt.toolMap[agenttool.ToolNameListBackgroundAgents]
	require.NotNil(t, listHandler, "list_background_agents must be registered on the runtime tool map")
	listCall := tools.ToolCall{
		ID:       "call_list_poll",
		Type:     "function",
		Function: tools.FunctionCall{Name: agenttool.ToolNameListBackgroundAgents},
	}
	var listOut string
	require.Eventually(t, func() bool {
		res, err := listHandler(t.Context(), sess, listCall, NewChannelSink(make(chan Event, 4)), tools.NopRuntime{})
		if err != nil {
			return false
		}
		listOut = res.Output
		return strings.Count(listOut, "Status:  completed") == 2
	}, 20*time.Second, time.Millisecond,
		"both background tasks must reach completed status via the real list handler")
	// Both the root-dispatched worker task and the worker-dispatched helper
	// task live in the same real handler.
	assert.Contains(t, listOut, "Agent:   worker")
	assert.Contains(t, listOut, "Agent:   helper")
	assert.Contains(t, listOut, taskID)

	// Turn 2: root's model inspects the finished task through the real
	// view handler; the task ID is dynamic, parsed from turn 1's result.
	rootProv.enqueue(
		newStreamBuilder().
			AddToolCallName("call_view_worker", agenttool.ToolNameViewBackgroundAgent).
			AddToolCallArguments("call_view_worker", fmt.Sprintf(`{"task_id":%q}`, taskID)).
			AddToolCallStopWithUsage(10, 5).
			Build(),
		newStreamBuilder().AddContent("background check complete").AddStopWithUsage(10, 5).Build(),
	)
	sess.AddMessage(session.UserMessage("check on the background task"))
	_, err = rt.Run(t.Context(), sess)
	require.NoError(t, err)

	viewOut := toolResultContent(t, sess, "call_view_worker")
	assert.Contains(t, viewOut, taskID)
	assert.Contains(t, viewOut, "Status:  completed", "the worker task must be completed")
	assert.Contains(t, viewOut, "worker done", "the worker's final output must be visible through view_background_agent")

	// Session structure: root → worker (pinned child) → helper (pinned
	// grandchild), with delegation lineage recorded per edge.
	child := firstSubSession(sess)
	require.NotNil(t, child, "root session must carry the worker sub-session")
	assert.Equal(t, "worker", child.AgentName, "background child session must be pinned to worker")
	assert.Equal(t, []string{"root"}, child.DelegationLineage)
	assert.Equal(t, "worker done", child.GetLastAssistantMessageContent())

	grandchild := firstSubSession(child)
	require.NotNil(t, grandchild, "worker session must carry the nested helper sub-session")
	assert.Equal(t, "helper", grandchild.AgentName, "nested background grandchild must be pinned to helper")
	assert.Equal(t, []string{"root", "worker"}, grandchild.DelegationLineage)
	assert.Equal(t, "nested helper done", grandchild.GetLastAssistantMessageContent())

	// Hook attribution: the nested helper completion belongs to worker (the
	// pinned caller), the worker completion to root.
	workerStops := rbWorker.snapshot()
	require.Len(t, workerStops, 1, "worker's subagent_stop hook must fire exactly once")
	assert.Equal(t, "helper", workerStops[0].AgentName)
	assert.Equal(t, child.ID, workerStops[0].ParentSessionID)
	assert.Equal(t, grandchild.ID, workerStops[0].SessionID)
	assert.Equal(t, "nested helper done", workerStops[0].StopResponse)

	rootStops := rbRoot.snapshot()
	require.Len(t, rootStops, 1, "root's subagent_stop hook must fire exactly once")
	assert.Equal(t, "worker", rootStops[0].AgentName)
	assert.Equal(t, sess.ID, rootStops[0].ParentSessionID)
	assert.Equal(t, child.ID, rootStops[0].SessionID)
	assert.Equal(t, "worker done", rootStops[0].StopResponse)

	assert.Equal(t, "root", rt.CurrentAgent().Name(),
		"the shared current agent must still be root after the whole chain")
}
