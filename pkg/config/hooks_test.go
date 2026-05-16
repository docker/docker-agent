package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
)

func TestHooksFromCLI_Empty(t *testing.T) {
	hooks := HooksFromCLI(nil, nil, nil, nil, nil, nil)
	assert.Nil(t, hooks)
}

func TestHooksFromCLI_SkipsEmptyCommands(t *testing.T) {
	// All empty/whitespace-only commands should be filtered out
	hooks := HooksFromCLI([]string{""}, []string{"  "}, []string{""}, []string{"  \t"}, nil, []string{"  "})
	assert.Nil(t, hooks)
}

func TestHooksFromCLI_MixedEmptyAndValid(t *testing.T) {
	hooks := HooksFromCLI([]string{"", "echo pre", "  "}, nil, []string{"echo start", ""}, nil, nil, nil)
	require.NotNil(t, hooks)

	require.Len(t, hooks.PreToolUse, 1)
	require.Len(t, hooks.PreToolUse[0].Hooks, 1)
	assert.Equal(t, "echo pre", hooks.PreToolUse[0].Hooks[0].Command)

	require.Len(t, hooks.SessionStart, 1)
	assert.Equal(t, "echo start", hooks.SessionStart[0].Command)
}

func TestHooksFromCLI_PreToolUse(t *testing.T) {
	hooks := HooksFromCLI([]string{"echo pre1", "echo pre2"}, nil, nil, nil, nil, nil)
	require.NotNil(t, hooks)

	require.Len(t, hooks.PreToolUse, 1)
	require.Len(t, hooks.PreToolUse[0].Hooks, 2)
	assert.Equal(t, "command", hooks.PreToolUse[0].Hooks[0].Type)
	assert.Equal(t, "echo pre1", hooks.PreToolUse[0].Hooks[0].Command)
	assert.Equal(t, "echo pre2", hooks.PreToolUse[0].Hooks[1].Command)
	// Matcher is empty string, which matches all tools by default
	assert.Empty(t, hooks.PreToolUse[0].Matcher)
}

func TestHooksFromCLI_AllTypes(t *testing.T) {
	hooks := HooksFromCLI(
		[]string{"pre-cmd"},
		[]string{"post-cmd"},
		[]string{"start-cmd"},
		[]string{"end-cmd"},
		[]string{"input-cmd"},
		[]string{"stop-cmd"},
	)
	require.NotNil(t, hooks)

	assert.Len(t, hooks.PreToolUse, 1)
	assert.Len(t, hooks.PostToolUse, 1)
	assert.Len(t, hooks.SessionStart, 1)
	assert.Len(t, hooks.SessionEnd, 1)
	assert.Len(t, hooks.OnUserInput, 1)
	assert.Len(t, hooks.Stop, 1)

	assert.Equal(t, "pre-cmd", hooks.PreToolUse[0].Hooks[0].Command)
	assert.Equal(t, "post-cmd", hooks.PostToolUse[0].Hooks[0].Command)
	assert.Equal(t, "start-cmd", hooks.SessionStart[0].Command)
	assert.Equal(t, "end-cmd", hooks.SessionEnd[0].Command)
	assert.Equal(t, "input-cmd", hooks.OnUserInput[0].Command)
	assert.Equal(t, "stop-cmd", hooks.Stop[0].Command)
}

func TestMergeHooks_BothNil(t *testing.T) {
	assert.Nil(t, MergeHooks(nil, nil))
}

func TestMergeHooks_CLINil(t *testing.T) {
	base := &latest.HooksConfig{
		SessionStart: []latest.HookDefinition{{Type: "command", Command: "echo base"}},
	}
	result := MergeHooks(base, nil)
	assert.Equal(t, base, result)
}

func TestMergeHooks_BaseNil(t *testing.T) {
	cli := &latest.HooksConfig{
		SessionStart: []latest.HookDefinition{{Type: "command", Command: "echo cli"}},
	}
	result := MergeHooks(nil, cli)
	assert.Equal(t, cli, result)
}

func TestMergeHooks_BothNonNil(t *testing.T) {
	base := &latest.HooksConfig{
		SessionStart: []latest.HookDefinition{{Type: "command", Command: "echo base"}},
		PreToolUse: []latest.HookMatcherConfig{{
			Matcher: "shell",
			Hooks:   []latest.HookDefinition{{Type: "command", Command: "echo base-pre"}},
		}},
	}
	cli := &latest.HooksConfig{
		SessionStart: []latest.HookDefinition{{Type: "command", Command: "echo cli"}},
		PreToolUse: []latest.HookMatcherConfig{{
			Hooks: []latest.HookDefinition{{Type: "command", Command: "echo cli-pre"}},
		}},
	}

	result := MergeHooks(base, cli)
	require.NotNil(t, result)

	// Session start hooks should be merged
	require.Len(t, result.SessionStart, 2)
	assert.Equal(t, "echo base", result.SessionStart[0].Command)
	assert.Equal(t, "echo cli", result.SessionStart[1].Command)

	// Pre tool use matchers should be merged
	require.Len(t, result.PreToolUse, 2)
	assert.Equal(t, "shell", result.PreToolUse[0].Matcher)
	assert.Equal(t, "echo base-pre", result.PreToolUse[0].Hooks[0].Command)
	assert.Empty(t, result.PreToolUse[1].Matcher)
	assert.Equal(t, "echo cli-pre", result.PreToolUse[1].Hooks[0].Command)
}

func TestMergeHooks_DoesNotMutateOriginals(t *testing.T) {
	base := &latest.HooksConfig{
		SessionStart: []latest.HookDefinition{{Type: "command", Command: "echo base"}},
	}
	cli := &latest.HooksConfig{
		SessionStart: []latest.HookDefinition{{Type: "command", Command: "echo cli"}},
	}

	result := MergeHooks(base, cli)

	// Originals should not be mutated
	assert.Len(t, base.SessionStart, 1)
	assert.Len(t, cli.SessionStart, 1)
	assert.Len(t, result.SessionStart, 2)
}

func TestRuntimeConfig_CLIHooks(t *testing.T) {
	rc := &RuntimeConfig{}
	assert.Nil(t, rc.CLIHooks())

	rc.HookSessionStart = []string{"echo start"}
	hooks := rc.CLIHooks()
	require.NotNil(t, hooks)
	assert.Len(t, hooks.SessionStart, 1)
	assert.Equal(t, "echo start", hooks.SessionStart[0].Command)
}

func TestRuntimeConfig_Clone_CopiesHooks(t *testing.T) {
	rc := &RuntimeConfig{}
	rc.HookPreToolUse = []string{"pre"}
	rc.HookPostToolUse = []string{"post"}
	rc.HookSessionStart = []string{"start"}
	rc.HookSessionEnd = []string{"end"}
	rc.HookOnUserInput = []string{"input"}
	rc.HookStop = []string{"stop"}

	clone := rc.Clone()
	assert.Equal(t, rc.HookPreToolUse, clone.HookPreToolUse)
	assert.Equal(t, rc.HookPostToolUse, clone.HookPostToolUse)
	assert.Equal(t, rc.HookSessionStart, clone.HookSessionStart)
	assert.Equal(t, rc.HookSessionEnd, clone.HookSessionEnd)
	assert.Equal(t, rc.HookOnUserInput, clone.HookOnUserInput)
	assert.Equal(t, rc.HookStop, clone.HookStop)

	// Mutating clone should not affect original
	clone.HookPreToolUse[0] = "changed"
	assert.Equal(t, "pre", rc.HookPreToolUse[0])
}
