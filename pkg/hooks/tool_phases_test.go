package hooks

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// failingHandlerRegistry returns a registry whose "fail" hook type
// reports the given handler outcome.
func failingHandlerRegistry(res HandlerResult, err error) *Registry {
	registry := NewRegistry()
	registry.Register("fail", func(HandlerEnv, Hook) (Handler, error) {
		return pipelineHandlerFunc(func(context.Context, []byte) (HandlerResult, error) {
			return res, err
		}), nil
	})
	return registry
}

func TestToolInputTransformComposesPatchesAndKeepsInputEventName(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	require.NoError(t, registry.RegisterBuiltin("append", func(_ context.Context, in *Input, args []string) (*Output, error) {
		assert.Equal(t, EventToolInputTransform, in.HookEventName)
		return &Output{HookSpecificOutput: &HookSpecificOutput{
			UpdatedInput: map[string]any{"cmd": in.ToolInput["cmd"].(string) + args[0]},
		}}, nil
	}))
	exec := NewExecutorWithRegistry(&Config{ToolInputTransform: []MatcherConfig{
		{Matcher: "other", Hooks: []Hook{{Type: HookTypeCommand, Command: "exit 2"}}},
		{Matcher: "shell", Hooks: []Hook{
			{Type: HookTypeCommand, Command: `echo '{"hook_specific_output":{"updated_input":{"cmd":"ls"}}}'`},
			{Type: HookTypeBuiltin, Command: "append", Args: []string{" -l"}},
		}},
		{Matcher: "*", Hooks: []Hook{{Type: HookTypeBuiltin, Command: "append", Args: []string{" -h"}}}},
	}}, t.TempDir(), nil, registry)
	original := map[string]any{"cmd": "original", "cwd": "work"}

	result, err := exec.Dispatch(t.Context(), EventToolInputTransform, &Input{ToolName: "shell", ToolInput: original})
	require.NoError(t, err)
	assert.True(t, result.Allowed)
	assert.Empty(t, result.Decision, "transform never carries a verdict")
	assert.Equal(t, map[string]any{"cmd": "ls -l -h", "cwd": "work"}, result.ModifiedInput)
	assert.Equal(t, "original", original["cmd"], "caller's input is not mutated")
}

// Transform failures follow on_error (default warn); exit-code semantics
// are unchanged from every other event.
func TestToolInputTransformFailurePolicy(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		result   HandlerResult
		err      error
		onError  string
		allowed  bool
		exitCode int
	}{
		{name: "error default warn", err: errors.New("boom"), allowed: true},
		{name: "error ignore", err: errors.New("boom"), onError: "ignore", allowed: true},
		{name: "error block", err: errors.New("boom"), onError: "block", allowed: false, exitCode: -1},
		{name: "exit 1", result: HandlerResult{ExitCode: 1}, allowed: true},
		{name: "exit 2", result: HandlerResult{ExitCode: 2, Stderr: "nope"}, allowed: false, exitCode: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			exec := pipelineTestExecutor(t, EventToolInputTransform, []Hook{
				{Type: HookTypeCommand, Command: `echo '{"hook_specific_output":{"updated_input":{"cmd":"first"}}}'`},
				{Type: "fail", Command: "fail", OnError: tc.onError},
			}, failingHandlerRegistry(tc.result, tc.err))

			result, err := exec.Dispatch(t.Context(), EventToolInputTransform, &Input{ToolInput: map[string]any{"cmd": "original"}})
			require.NoError(t, err)
			assert.Equal(t, tc.allowed, result.Allowed)
			assert.Equal(t, tc.exitCode, result.ExitCode)
			assert.Equal(t, "first", result.ModifiedInput["cmd"], "earlier rewrite survives the failure")
			if tc.onError == "block" {
				assert.Contains(t, result.Message, "tool_input_transform hook failed to execute")
			}
		})
	}
}

func TestToolGuardAggregatesMostRestrictiveVerdict(t *testing.T) {
	t.Parallel()

	guard := func(decision Decision, reason, meta string) Hook {
		return Hook{Type: HookTypeBuiltin, Command: "verdict", Args: []string{string(decision), reason, meta}}
	}
	registry := NewRegistry()
	require.NoError(t, registry.RegisterBuiltin("verdict", func(_ context.Context, in *Input, args []string) (*Output, error) {
		assert.Equal(t, EventToolGuard, in.HookEventName)
		assert.Equal(t, "original", in.ToolInput["cmd"], "guards see the same input; no pipeline")
		var meta map[string]string
		if args[2] != "" {
			meta = map[string]string{"from_" + args[0]: args[2], "shared": args[2]}
		}
		return &Output{HookSpecificOutput: &HookSpecificOutput{
			PermissionDecision:       Decision(args[0]),
			PermissionDecisionReason: args[1],
			Metadata:                 meta,
			UpdatedInput:             map[string]any{"cmd": "rewritten by " + args[0]},
		}}, nil
	}))

	for _, tc := range []struct {
		name        string
		hooks       []Hook
		wantVerdict Decision
		wantReason  string
		wantAllowed bool
	}{
		{
			name:        "allow is advisory",
			hooks:       []Hook{guard(DecisionAllow, "fine", "")},
			wantVerdict: DecisionAllow, wantReason: "fine", wantAllowed: true,
		},
		{
			name:        "ask beats allow and keeps Allowed",
			hooks:       []Hook{guard(DecisionAllow, "fine", ""), guard(DecisionAsk, "unsure", "")},
			wantVerdict: DecisionAsk, wantReason: "unsure", wantAllowed: true,
		},
		{
			name:        "deny beats ask and denies",
			hooks:       []Hook{guard(DecisionAsk, "unsure", ""), guard(DecisionDeny, "destructive", ""), guard(DecisionAllow, "fine", "")},
			wantVerdict: DecisionDeny, wantReason: "destructive", wantAllowed: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			exec := pipelineTestExecutor(t, EventToolGuard, tc.hooks, registry)
			result, err := exec.Dispatch(t.Context(), EventToolGuard, &Input{ToolName: "shell", ToolInput: map[string]any{"cmd": "original"}})
			require.NoError(t, err)
			assert.Equal(t, tc.wantVerdict, result.Decision)
			assert.Equal(t, tc.wantReason, result.DecisionReason)
			assert.Equal(t, tc.wantAllowed, result.Allowed)
			assert.False(t, result.PermissionAllowed, "guard allow never auto-approves")
			assert.Nil(t, result.ModifiedInput, "guards cannot rewrite input")
			if tc.wantVerdict == DecisionDeny {
				assert.Contains(t, result.Message, "destructive")
			}
		})
	}

	t.Run("metadata merges last wins", func(t *testing.T) {
		t.Parallel()
		exec := pipelineTestExecutor(t, EventToolGuard, []Hook{
			guard(DecisionAsk, "a", "one"),
			guard(DecisionAllow, "b", "two"),
		}, registry)
		result, err := exec.Dispatch(t.Context(), EventToolGuard, &Input{ToolName: "shell", ToolInput: map[string]any{"cmd": "original"}})
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"from_ask": "one", "from_allow": "two", "shared": "two"}, result.Metadata)
	})
}

// Guard failures, including unexpected exit codes, fail closed regardless of on_error.
func TestToolGuardFailsClosed(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		result   HandlerResult
		err      error
		onError  string
		allowed  bool
		exitCode int
	}{
		{name: "error default", err: errors.New("boom"), allowed: false, exitCode: -1},
		{name: "error ignore still denies", err: errors.New("boom"), onError: "ignore", allowed: false, exitCode: -1},
		{name: "canceled", err: context.Canceled, allowed: false, exitCode: -1},
		{name: "exit 1", result: HandlerResult{ExitCode: 1}, allowed: false, exitCode: -1},
		{name: "exit 2", result: HandlerResult{ExitCode: 2, Stderr: "nope"}, allowed: false, exitCode: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			exec := pipelineTestExecutor(t, EventToolGuard, []Hook{
				{Type: HookTypeCommand, Command: `echo '{"hook_specific_output":{"permission_decision":"allow","permission_decision_reason":"fine"}}'`},
				{Type: "fail", Command: "fail", OnError: tc.onError},
			}, failingHandlerRegistry(tc.result, tc.err))

			result, err := exec.Dispatch(t.Context(), EventToolGuard, &Input{ToolName: "shell"})
			require.NoError(t, err)
			assert.Equal(t, tc.allowed, result.Allowed)
			assert.Equal(t, tc.exitCode, result.ExitCode)
			assert.Equal(t, DecisionAllow, result.Decision, "other hooks' verdicts still aggregate")
			if tc.err != nil {
				assert.Contains(t, result.Message, "tool_guard hook failed to execute")
			}
		})
	}
}

// A payload the executor cannot serialize must block every block-capable event.
func TestDispatchSerializationFailureBlocksPreApprovalEvents(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		PreToolUse:         []MatcherConfig{{Hooks: trueHook}, {Hooks: trueHook, PreemptYolo: new(true)}},
		ToolInputTransform: []MatcherConfig{{Hooks: trueHook}},
		ToolGuard:          []MatcherConfig{{Hooks: trueHook}},
	}
	exec := NewExecutor(cfg, t.TempDir(), nil)
	unserializable := map[string]any{"bad": make(chan int)}

	for _, event := range []EventType{EventToolInputTransform, EventToolGuard, EventPreToolUsePreYolo, EventPreToolUse} {
		result, err := exec.Dispatch(t.Context(), event, &Input{ToolName: "shell", ToolInput: unserializable})
		require.NoError(t, err, event)
		require.NotNil(t, result, event)
		assert.False(t, result.Allowed, event)
		assert.Equal(t, -1, result.ExitCode, event)
		assert.Contains(t, result.Message, "failed to serialize hook input", event)
	}
}
