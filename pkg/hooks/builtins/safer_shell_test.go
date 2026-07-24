package builtins

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/safety"
)

func saferShellInput(cmd string) *hooks.Input {
	return &hooks.Input{
		HookEventName: hooks.EventPreToolUse,
		ToolName:      safety.ShellToolName,
		ToolInput:     map[string]any{"cmd": cmd},
	}
}

// The builtin is a pure labeller: it must attach classification
// metadata without emitting any permission decision, whatever the
// command looks like.
func TestSaferShell_LabelsWithoutVerdict(t *testing.T) {
	tests := []struct {
		name        string
		cmd         string
		safetyLabel string
		blastRadius string
	}{
		{"destructive", "rm -rf /tmp/foo", "destructive", "high"},
		{"safe", "git status", "safe", "safe"},
		{"unknown", "./deploy.sh", "unknown", "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := saferShell(t.Context(), saferShellInput(tt.cmd), nil)
			require.NoError(t, err)
			require.NotNil(t, out)
			require.NotNil(t, out.HookSpecificOutput)

			assert.Empty(t, out.HookSpecificOutput.PermissionDecision,
				"labeller must not emit a permission decision")
			meta := out.HookSpecificOutput.Metadata
			assert.Equal(t, tt.safetyLabel, meta[safety.MetaSafetyLabel])
			assert.Equal(t, tt.blastRadius, meta[safety.MetaBlastRadius])
			if tt.safetyLabel != "unknown" {
				assert.NotEmpty(t, meta[safety.MetaReason])
			}
		})
	}
}

func TestSaferShell_AcceptsCommandAliasKey(t *testing.T) {
	out, err := saferShell(t.Context(), &hooks.Input{
		HookEventName: hooks.EventPreToolUse,
		ToolName:      safety.ShellToolName,
		ToolInput:     map[string]any{"command": "rm -rf /tmp/x"},
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, "destructive", out.HookSpecificOutput.Metadata[safety.MetaSafetyLabel])
}

func TestSaferShell_NoOpForNonShellTool(t *testing.T) {
	out, err := saferShell(t.Context(), &hooks.Input{
		HookEventName: hooks.EventPreToolUse,
		ToolName:      "read_file",
		ToolInput:     map[string]any{"path": "/tmp/x"},
	}, nil)
	require.NoError(t, err)
	assert.Nil(t, out)
}

func TestSaferShell_NoOpUnderWrongEvent(t *testing.T) {
	out, err := saferShell(t.Context(), &hooks.Input{
		HookEventName: hooks.EventPostToolUse,
		ToolName:      safety.ShellToolName,
		ToolInput:     map[string]any{"cmd": "rm -rf /"},
	}, nil)
	require.NoError(t, err)
	assert.Nil(t, out)
}

func TestSaferShell_NilInputIsNoOp(t *testing.T) {
	out, err := saferShell(t.Context(), nil, nil)
	require.NoError(t, err)
	assert.Nil(t, out)
}

func TestSaferShell_MissingCommandLabelsUnknown(t *testing.T) {
	out, err := saferShell(t.Context(), &hooks.Input{
		HookEventName: hooks.EventPreToolUse,
		ToolName:      safety.ShellToolName,
		ToolInput:     map[string]any{},
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, "unknown", out.HookSpecificOutput.Metadata[safety.MetaSafetyLabel])
}
