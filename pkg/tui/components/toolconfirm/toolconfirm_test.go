package toolconfirm

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
)

func shellCall(cmd string) tools.ToolCall {
	return tools.ToolCall{
		Function: tools.FunctionCall{
			Name:      "shell",
			Arguments: `{"cmd":` + jsonString(cmd) + `}`,
		},
	}
}

func jsonString(s string) string {
	return `"` + s + `"`
}

func TestBuildPermissionPattern(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		call tools.ToolCall
		want string
	}{
		{
			name: "shell extracts the command",
			call: shellCall("ls -la /tmp"),
			want: "shell:cmd=ls*",
		},
		{
			name: "shell with single word",
			call: shellCall("ls"),
			want: "shell:cmd=ls*",
		},
		{
			name: "shell with leading whitespace and newlines",
			call: shellCall(`\tgit\nstatus`), // decodes to "\tgit\nstatus"
			want: "shell:cmd=git*",
		},
		{
			name: "shell with empty command falls back to tool name",
			call: shellCall(""),
			want: "shell",
		},
		{
			name: "shell with invalid arguments falls back to tool name",
			call: tools.ToolCall{Function: tools.FunctionCall{Name: "shell", Arguments: "not json"}},
			want: "shell",
		},
		{
			name: "other tools use the tool name",
			call: tools.ToolCall{Function: tools.FunctionCall{Name: "write_file", Arguments: `{"path":"/x"}`}},
			want: "write_file",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, BuildPermissionPattern(tt.call))
		})
	}
}

func TestAlwaysAllowLabel(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "always allow ls*", AlwaysAllowLabel("shell:cmd=ls*"))
	assert.Equal(t, "always allow write_file", AlwaysAllowLabel("write_file"))
}

func TestOptionsHelpUsesThePattern(t *testing.T) {
	t.Parallel()
	opts := OptionsHelp("shell:cmd=rm*")
	require.Len(t, opts, 8)
	assert.Equal(t, []string{"Y", "yes", "N", "no", "T", "always allow rm*", "A", "all tools"}, opts)
}

func TestDecisionResume(t *testing.T) {
	t.Parallel()
	assert.Equal(t, runtime.ResumeApprove(), Approve.Resume("", ""))
	assert.Equal(t, runtime.ResumeApproveTool("shell:cmd=ls*"), ApproveTool.Resume("shell:cmd=ls*", ""))
	assert.Equal(t, runtime.ResumeApproveAutonomous(), ApproveAutonomous.Resume("", ""))
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
		{"a", ApproveAutonomous},
		{"A", ApproveAutonomous},
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

	for i, want := range []Decision{Approve, Reject, ApproveTool, ApproveAutonomous} {
		action := string(ActionKeys[i])
		decision, ok := DecisionForAction(action)
		require.True(t, ok, "action %q", action)
		assert.Equal(t, want, decision, "action %q", action)
	}

	_, ok := DecisionForAction("x")
	assert.False(t, ok)
}
