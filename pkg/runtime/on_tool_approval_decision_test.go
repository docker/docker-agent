package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// runtimeWithRecordedToolApproval mirrors the runtimeWithRecorded*
// helpers for the on_tool_approval_decision event. Same pattern: a
// recording builtin on the runtime's private registry so the test
// can assert on the dispatched verdict + source without exposing a
// production Opt that would tempt users to inject builtins ad hoc.
func runtimeWithRecordedToolApproval(t *testing.T) (*LocalRuntime, *recordingBuiltin) {
	t.Helper()

	rb := &recordingBuiltin{}
	prov := &mockProvider{id: "test/mock-model", stream: &mockStream{}}
	a := agent.New("root", "instructions",
		agent.WithModel(prov),
		agent.WithHooks(&hooks.Config{
			OnToolApprovalDecision: []hooks.Hook{{
				Type:    hooks.HookTypeBuiltin,
				Command: "test_record_tool_approval",
			}},
		}),
	)
	tm := team.New(team.WithAgents(a))

	r, err := NewLocalRuntime(tm, WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	require.NoError(t, r.hooksRegistry.RegisterBuiltin("test_record_tool_approval", rb.hook))
	r.buildHooksExecutors()

	return r, rb
}

// TestExecuteOnToolApprovalDecisionHooks_ForwardsVerdictAndSource pins
// the contract: the dispatched Input carries the verdict and source
// classifier verbatim, plus the tool-call identifying fields the
// existing PreToolUse / PostToolUse hooks already use. That gives
// audit pipelines a uniform "tool call X resulted in verdict Y from
// source Z" record across the whole approval chain.
func TestExecuteOnToolApprovalDecisionHooks_ForwardsVerdictAndSource(t *testing.T) {
	t.Parallel()

	r, rb := runtimeWithRecordedToolApproval(t)
	a := r.CurrentAgent()
	require.NotNil(t, a)

	sess := &session.Session{ID: "session-z"}
	tc := tools.ToolCall{
		ID: "call-1",
		Function: tools.FunctionCall{
			Name:      "read_file",
			Arguments: `{"path":"/tmp/x"}`,
		},
	}
	r.executeOnToolApprovalDecisionHooks(t.Context(), sess, a, tc, ApprovalDecisionAllow, ApprovalSourceReadOnlyHint)

	got := rb.snapshot()
	require.Len(t, got, 1)
	in := got[0]
	assert.Equal(t, "read_file", in.ToolName)
	assert.Equal(t, "call-1", in.ToolUseID)
	assert.Equal(t, ApprovalDecisionAllow, in.ApprovalDecision)
	assert.Equal(t, ApprovalSourceReadOnlyHint, in.ApprovalSource)
}

// TestApprovalSourceMappersAreStable pins the stable classifier
// strings used by [allowSourceFor] and [denySourceFor]. Tests that
// the team-permissions vs session-permissions split (today: by
// checker.source string match) survives changes to the inner labels.
func TestApprovalSourceMappersAreStable(t *testing.T) {
	t.Parallel()

	assert.Equal(t, ApprovalSourceSessionPermissionsAllow, allowSourceFor("session permissions"))
	assert.Equal(t, ApprovalSourceTeamPermissionsAllow, allowSourceFor("permissions configuration"))
	assert.Equal(t, ApprovalSourceTeamPermissionsAllow, allowSourceFor("anything-else"),
		"unknown source must default to team_permissions to avoid silent misclassification on future label changes")

	assert.Equal(t, ApprovalSourceSessionPermissionsDeny, denySourceFor("session permissions"))
	assert.Equal(t, ApprovalSourceTeamPermissionsDeny, denySourceFor("permissions configuration"))
}
