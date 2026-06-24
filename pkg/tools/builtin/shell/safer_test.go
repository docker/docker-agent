package shell

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

func TestAssessDestructiveShellCommand(t *testing.T) {
	tests := []struct {
		name        string
		command     string
		destructive bool
		level       tools.BlastRadiusLevel
	}{
		{name: "rm rf", command: "rm -rf /tmp/x", destructive: true, level: tools.BlastRadiusHigh},
		{name: "rm recursive", command: "rm -r /tmp/x", destructive: true, level: tools.BlastRadiusHigh},
		{name: "rm force file", command: "rm -f /tmp/x", destructive: true, level: tools.BlastRadiusMedium},
		{name: "plain rm file", command: "rm /tmp/x", destructive: true, level: tools.BlastRadiusLow},
		{name: "find delete", command: "find . -delete", destructive: true, level: tools.BlastRadiusHigh},
		{name: "docker compose down volumes", command: "docker compose down --volumes", destructive: true, level: tools.BlastRadiusHigh},
		{name: "docker system prune", command: "docker system prune", destructive: true, level: tools.BlastRadiusMedium},
		{name: "git reset out of scope is embedded", command: "git reset --hard", destructive: true, level: tools.BlastRadiusHigh},
		{name: "normal command", command: "ls -la", destructive: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := assessDestructiveShellCommand(tt.command)
			if !tt.destructive {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.True(t, got.Destructive)
			assert.Equal(t, tt.level, got.BlastRadius)
		})
	}
}

func TestSafetyPatternsLoad(t *testing.T) {
	patterns, err := loadSafetyPatterns()
	require.NoError(t, err)
	assert.Greater(t, len(patterns), 50)
}

func TestValidateShellToolCallRespectsSaferFlag(t *testing.T) {
	args, err := json.Marshal(RunShellArgs{Cmd: "rm -rf /tmp/x"})
	require.NoError(t, err)
	call := tools.ToolCall{Function: tools.FunctionCall{Name: ToolNameShell, Arguments: string(args)}}

	assert.Nil(t, (&shellHandler{}).ValidateShellToolCall(call))

	safety := (&shellHandler{safer: true}).ValidateShellToolCall(call)
	require.NotNil(t, safety)
	assert.True(t, safety.Destructive)
	assert.Equal(t, tools.BlastRadiusHigh, safety.BlastRadius)
}

// The autonomous estimator recognizes ls as read-only, so safer mode no
// longer force-gates it: the validator returns nil and the call goes
// through the normal approval flow.
func TestValidateShellToolCallSaferPassesReadOnlyCommand(t *testing.T) {
	args, err := json.Marshal(RunShellArgs{Cmd: "ls -la"})
	require.NoError(t, err)
	call := tools.ToolCall{Function: tools.FunctionCall{Name: ToolNameShell, Arguments: string(args)}}

	safety := (&shellHandler{safer: true}).ValidateShellToolCall(call)
	assert.Nil(t, safety)
}

// A command the estimator cannot recognize still falls back to a
// fail-closed safer-mode confirmation with an unknown blast radius.
func TestValidateShellToolCallSaferWarnsForUnrecognizedCommand(t *testing.T) {
	args, err := json.Marshal(RunShellArgs{Cmd: "frobnicate --wibble foo"})
	require.NoError(t, err)
	call := tools.ToolCall{Function: tools.FunctionCall{Name: ToolNameShell, Arguments: string(args)}}

	safety := (&shellHandler{safer: true}).ValidateShellToolCall(call)
	require.NotNil(t, safety)
	assert.True(t, safety.Destructive)
	assert.Equal(t, tools.BlastRadiusUnknown, safety.BlastRadius)
	assert.Contains(t, safety.Reason, "safer-mode")
}

// The estimator produces a real blast-radius tier for a recognized
// destructive command that misses the curated pattern set (blkdiscard is
// not in safety_patterns.json), instead of the old blanket
// BlastRadiusUnknown.
func TestValidateShellToolCallSaferEstimatesUnpatternedDestructive(t *testing.T) {
	args, err := json.Marshal(RunShellArgs{Cmd: "blkdiscard /dev/sdb"})
	require.NoError(t, err)
	call := tools.ToolCall{Function: tools.FunctionCall{Name: ToolNameShell, Arguments: string(args)}}

	// Sanity: the command really is a pattern miss, so this exercises the
	// estimator and not assessDestructiveShellCommand.
	require.Nil(t, assessDestructiveShellCommand("blkdiscard /dev/sdb"))

	safety := (&shellHandler{safer: true}).ValidateShellToolCall(call)
	require.NotNil(t, safety)
	assert.True(t, safety.Destructive)
	assert.Equal(t, tools.BlastRadiusHigh, safety.BlastRadius)
}
