package toolconfirm

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/permissions"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
)

// The pattern logic is tested in pkg/permissions; this guards the delegation.
func TestPermissionHelpersDelegate(t *testing.T) {
	t.Parallel()
	call := tools.ToolCall{Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"ls -la"}`}}
	assert.Equal(t, permissions.BuildPermissionPattern(t.Context(), call), BuildPermissionPattern(call))
	assert.Equal(t, permissions.AlwaysAllowLabel("shell:cmd=ls*"), AlwaysAllowLabel("shell:cmd=ls*"))
}

func TestOptionsHelpUsesThePattern(t *testing.T) {
	t.Parallel()
	opts := OptionsHelp("shell:cmd=rm*")
	require.Len(t, opts, 10)
	assert.Equal(t, []string{"Y", "yes", "N", "no", "T", "always allow rm*", "B", "balanced", "A", "all tools"}, opts)
}

func TestDecisionResume(t *testing.T) {
	t.Parallel()
	assert.Equal(t, runtime.ResumeApprove(), Approve.Resume("", ""))
	assert.Equal(t, runtime.ResumeApproveTool("shell:cmd=ls*"), ApproveTool.Resume("shell:cmd=ls*", ""))
	assert.Equal(t, runtime.ResumeApproveBalanced(), ApproveBalanced.Resume("", ""))
	assert.Equal(t, runtime.ResumeApproveAutonomous(), ApproveSession.Resume("", ""))
	assert.Equal(t, runtime.ResumeReject("too risky"), Reject.Resume("", "too risky"))
}

func TestRejectionReasonsAreStable(t *testing.T) {
	t.Parallel()
	reasons := RejectionReasons()
	require.Len(t, reasons, 4)
	ids := make([]string, 0, len(reasons))
	for _, r := range reasons {
		require.NotEmpty(t, r.Label)
		require.NotEmpty(t, r.Value)
		ids = append(ids, r.ID)
	}
	assert.Equal(t, []string{"bad_args", "wrong_tool", "unsafe", "clarify"}, ids)
}

func TestKeyMapDecisionFor(t *testing.T) {
	t.Parallel()

	keyMap := DefaultKeyMap()
	for _, tt := range []struct {
		key      string
		decision Decision
	}{
		{"y", Approve},
		{"Y", Approve},
		{"n", Reject},
		{"N", Reject},
		{"t", ApproveTool},
		{"T", ApproveTool},
		{"a", ApproveSession},
		{"A", ApproveSession},
	} {
		decision, ok := keyMap.DecisionFor(tea.KeyPressMsg{Code: rune(tt.key[0]), Text: tt.key})
		require.True(t, ok, "key %q", tt.key)
		assert.Equal(t, tt.decision, decision, "key %q", tt.key)
	}

	_, ok := keyMap.DecisionFor(tea.KeyPressMsg{Code: 'x', Text: "x"})
	assert.False(t, ok)
}

func TestDecisionForAction(t *testing.T) {
	t.Parallel()

	for i, want := range []Decision{Approve, Reject, ApproveTool, ApproveBalanced, ApproveSession} {
		action := string(ActionKeys[i])
		decision, ok := DecisionForAction(action)
		require.True(t, ok, "action %q", action)
		assert.Equal(t, want, decision, "action %q", action)
	}

	_, ok := DecisionForAction("x")
	assert.False(t, ok)
}
