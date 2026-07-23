package builtins

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/hooks"
)

// safer_shell is now a pure labeller: it always returns an Allow
// verdict for shell tool calls with classification metadata attached.
// The runtime's (mode × label) verdict table is what actually gates
// the call — see pkg/runtime/toolexec.

// Destructive fixtures produce an Allow verdict with the expected
// blast-radius level in metadata. Metadata carries blast_radius +
// category + reason.
func TestSaferShell_MatchesDestructivePatterns(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		cmd         string
		wantLevel   string
		wantPattern string
	}{
		{"rm -rf path", "rm -rf /tmp/x", "high", "rm -rf"},
		{"rm -r path", "rm -r /tmp/x", "high", "rm -r"},
		{"docker volume rm", "docker volume rm cache", "high", "docker volume rm"},
		{"docker system prune all volumes", "docker system prune -af --volumes", "high", "docker system prune"},
		{"mkfs", "mkfs.ext4 /dev/sda1", "high", "mkfs"},
		{"rmdir empty", "rmdir /tmp/x", "low", "rmdir"},
		{"chmod recursive 777", "chmod -R 777 /tmp/x", "low", "chmod -R 777"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := saferShell(t.Context(), &hooks.Input{
				HookEventName: hooks.EventPreToolUse,
				ToolName:      shellToolName,
				ToolInput:     map[string]any{"cmd": tc.cmd},
			}, nil)
			require.NoError(t, err)
			require.NotNil(t, out, "destructive command %q should produce metadata", tc.cmd)
			require.NotNil(t, out.HookSpecificOutput)
			assert.Equal(t, hooks.EventPreToolUse, out.HookSpecificOutput.HookEventName)
			assert.Equal(t, hooks.DecisionAllow, out.HookSpecificOutput.PermissionDecision,
				"classifier is a pure labeller — always Allow")
			assert.Equal(t, tc.wantLevel, out.HookSpecificOutput.Metadata[metaBlastRadius],
				"unexpected blast radius for %q", tc.cmd)
			assert.Contains(t, out.HookSpecificOutput.PermissionDecisionReason, tc.wantPattern,
				"reason should name the matched pattern")
			assert.NotEmpty(t, out.HookSpecificOutput.Metadata[metaCategory],
				"destructive matches must carry a category tag")
		})
	}
}

// Safe patterns produce an Allow verdict with blast_radius=safe.
func TestSaferShell_SafeCommandsProduceSafeRadius(t *testing.T) {
	t.Parallel()

	safeCases := []string{
		"ls",
		"ls -la",
		"cat README.md",
		"pwd",
		"git status",
		"docker ps",
		"kubectl get pods",
	}
	for _, cmd := range safeCases {
		t.Run(cmd, func(t *testing.T) {
			out, err := saferShell(t.Context(), &hooks.Input{
				HookEventName: hooks.EventPreToolUse,
				ToolName:      shellToolName,
				ToolInput:     map[string]any{"cmd": cmd},
			}, nil)
			require.NoError(t, err, "safe command %q must produce a verdict", cmd)
			require.NotNil(t, out)
			require.NotNil(t, out.HookSpecificOutput)
			assert.Equal(t, hooks.DecisionAllow, out.HookSpecificOutput.PermissionDecision,
				"classifier is a pure labeller — always Allow")
			assert.Equal(t, radiusSafe, out.HookSpecificOutput.Metadata[metaBlastRadius])
			assert.Contains(t, out.HookSpecificOutput.PermissionDecisionReason, "safe read-only pattern")
		})
	}
}

// Compound shell: destructive matches short-circuit first (worst
// wins); otherwise the compound is rejected wholesale by
// [bestSafeMatch] and falls through to unknown, since a single-line
// compound can smuggle unsafe segments past a naive per-segment
// check.
func TestSaferShell_CompoundShellRadius(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		cmd        string
		wantRadius string
	}{
		{"safe-then-destructive AND", "cd /tmp && rm -rf foo", "high"},
		{"safe-then-destructive semicolon", "cd /tmp; rm -rf foo", "high"},
		{"safe-then-destructive pipe", "find /tmp | xargs rm -rf", "low"},
		{"two safes chained via && stay unknown", "ls && pwd", "unknown"},
		{"safe OR safe stays unknown", "git status || git diff", "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := saferShell(t.Context(), &hooks.Input{
				HookEventName: hooks.EventPreToolUse,
				ToolName:      shellToolName,
				ToolInput:     map[string]any{"cmd": tc.cmd},
			}, nil)
			require.NoError(t, err)
			require.NotNil(t, out)
			require.NotNil(t, out.HookSpecificOutput)
			assert.Equal(t, hooks.DecisionAllow, out.HookSpecificOutput.PermissionDecision)
			assert.Equal(t, tc.wantRadius, out.HookSpecificOutput.Metadata[metaBlastRadius])
		})
	}
}

// The "command" alias for the canonical "cmd" arg — the shell tool
// accepts both. Without this the alias path would silently bypass
// classification.
func TestSaferShell_AcceptsCommandAliasKey(t *testing.T) {
	t.Parallel()

	out, err := saferShell(t.Context(), &hooks.Input{
		HookEventName: hooks.EventPreToolUse,
		ToolName:      shellToolName,
		ToolInput:     map[string]any{"command": "rm -rf /tmp/x"},
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, "high", out.HookSpecificOutput.Metadata[metaBlastRadius])
}

// The builtin is registered under matcher "*", so it sees every
// pre_tool_use dispatch. It must return nil for non-shell tools.
func TestSaferShell_NoOpForNonShellTool(t *testing.T) {
	t.Parallel()

	out, err := saferShell(t.Context(), &hooks.Input{
		HookEventName: hooks.EventPreToolUse,
		ToolName:      "filesystem",
		ToolInput:     map[string]any{"cmd": "rm -rf /tmp/x"},
	}, nil)
	require.NoError(t, err)
	assert.Nil(t, out)
}

// The builtin only acts on EventPreToolUse. Anywhere else it no-ops.
func TestSaferShell_NoOpUnderWrongEvent(t *testing.T) {
	t.Parallel()

	out, err := saferShell(t.Context(), &hooks.Input{
		HookEventName: hooks.EventPostToolUse,
		ToolName:      shellToolName,
		ToolInput:     map[string]any{"cmd": "rm -rf /tmp/x"},
	}, nil)
	require.NoError(t, err)
	assert.Nil(t, out)
}

// A shell command that matches neither destructive nor safe patterns
// produces an Allow verdict with blast_radius=unknown so the runtime
// still sees a label to feed the mode table.
func TestSaferShell_UnknownCommandProducesUnknownRadius(t *testing.T) {
	t.Parallel()

	out, err := saferShell(t.Context(), &hooks.Input{
		HookEventName: hooks.EventPreToolUse,
		ToolName:      shellToolName,
		ToolInput:     map[string]any{"cmd": "myproject-cli deploy --prod"},
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, out)
	require.NotNil(t, out.HookSpecificOutput)
	assert.Equal(t, hooks.DecisionAllow, out.HookSpecificOutput.PermissionDecision)
	assert.Equal(t, "unknown", out.HookSpecificOutput.Metadata[metaBlastRadius])
}

// Empty or missing cmd / command keys produce Allow + blast_radius=unknown.
func TestSaferShell_EmptyOrMissingCommandProducesUnknown(t *testing.T) {
	t.Parallel()

	cases := []map[string]any{
		nil,
		{},
		{"cmd": ""},
		{"unrelated": "rm -rf /tmp"},
	}
	for i, in := range cases {
		out, err := saferShell(t.Context(), &hooks.Input{
			HookEventName: hooks.EventPreToolUse,
			ToolName:      shellToolName,
			ToolInput:     in,
		}, nil)
		require.NoError(t, err, "case %d", i)
		require.NotNil(t, out, "case %d: %v", i, in)
		require.NotNil(t, out.HookSpecificOutput)
		assert.Equal(t, hooks.DecisionAllow, out.HookSpecificOutput.PermissionDecision, "case %d", i)
		assert.Equal(t, "unknown", out.HookSpecificOutput.Metadata[metaBlastRadius], "case %d", i)
	}
}

// Nil input passes through with no output (defensive).
func TestSaferShell_NilInputIsNoOp(t *testing.T) {
	t.Parallel()

	out, err := saferShell(t.Context(), nil, nil)
	require.NoError(t, err)
	assert.Nil(t, out)
}

// ApplyAgentDefaults must still auto-inject the classifier with
// preempt_yolo:true so its metadata is available to the runtime
// before Decide().
func TestSaferShell_ApplyAgentDefaultsAutoInjectsBuiltin(t *testing.T) {
	t.Parallel()

	cfg := ApplyAgentDefaults(nil, AgentDefaults{SaferShell: true})
	require.NotNil(t, cfg, "SaferShell=true must produce a non-empty config")
	require.Len(t, cfg.PreToolUse, 1, "expected exactly one pre_tool_use matcher entry")
	mc := cfg.PreToolUse[0]
	assert.Equal(t, "*", mc.Matcher,
		"wildcard matcher keeps the hook generic so other pre_tool_use hooks can coexist")
	require.NotNil(t, mc.PreemptYolo, "preempt_yolo must be set on the auto-injected entry")
	assert.True(t, *mc.PreemptYolo,
		"preempt_yolo must be true so metadata reaches the dispatcher before the mode-verdict step")
	require.Len(t, mc.Hooks, 1)
	assert.Equal(t, hooks.HookTypeBuiltin, mc.Hooks[0].Type)
	assert.Equal(t, SaferShell, mc.Hooks[0].Command)
}

// LabelFromBlastRadius maps every wire-format radius onto the runtime
// classifier label. safe → safe; low/medium/high → destructive;
// anything else → unknown.
func TestLabelFromBlastRadius(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"safe":    "safe",
		"low":     "destructive",
		"medium":  "destructive",
		"high":    "destructive",
		"unknown": "unknown",
		"":        "unknown",
		"garbage": "unknown",
	}
	for radius, wantLabel := range cases {
		t.Run(radius, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, wantLabel, LabelFromBlastRadius(radius))
		})
	}
}
