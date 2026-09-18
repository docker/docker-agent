package hooks

import (
	"context"
	"maps"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Identical definitions collapse; different builtin arguments remain distinct.
func TestExecutorDedupsIdenticalDefinitions(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	registry := NewRegistry()
	require.NoError(t, registry.RegisterBuiltin("count", func(_ context.Context, _ *Input, _ []string) (*Output, error) {
		calls.Add(1)
		return nil, nil
	}))

	cfg := &Config{
		SessionStart: []Hook{
			// The next two are structurally identical and must collapse.
			{Type: HookTypeBuiltin, Command: "count", Args: []string{"a"}},
			{Type: HookTypeBuiltin, Command: "count", Args: []string{"a"}},
			// Same name, different Args -> distinct invocation.
			{Type: HookTypeBuiltin, Command: "count", Args: []string{"b"}},
			// No-args version -> also distinct from both above.
			{Type: HookTypeBuiltin, Command: "count"},
		},
	}

	exec := NewExecutorWithRegistry(cfg, t.TempDir(), nil, registry)
	_, err := exec.Dispatch(t.Context(), EventSessionStart, &Input{SessionID: "s"})
	require.NoError(t, err)

	// Three distinct definitions produce three invocations.
	assert.Equal(t, int32(3), calls.Load())
}

func TestHookIdentityIncludesEveryField(t *testing.T) {
	t.Parallel()

	base := Hook{
		Name: "hook", Type: HookTypeModel, Command: "command", Args: []string{"arg"},
		Timeout: 5, Env: map[string]string{"PROFILE": "first"}, WorkingDir: "first",
		OnError: "warn", Model: "test/first", Prompt: "first", Schema: "first",
	}
	changes := map[string]func(*Hook){
		"Name":         func(h *Hook) { h.Name = "other" },
		"Type":         func(h *Hook) { h.Type = HookTypeCommand },
		"Command":      func(h *Hook) { h.Command = "other" },
		"Args":         func(h *Hook) { h.Args = []string{"other"} },
		"Timeout":      func(h *Hook) { h.Timeout = 10 },
		"Env":          func(h *Hook) { h.Env = map[string]string{"PROFILE": "other"} },
		"WorkingDir":   func(h *Hook) { h.WorkingDir = "other" },
		"OnError":      func(h *Hook) { h.OnError = "block" },
		"Model":        func(h *Hook) { h.Model = "test/other" },
		"Prompt":       func(h *Hook) { h.Prompt = "other" },
		"Schema":       func(h *Hook) { h.Schema = "other" },
		"StrictOutput": func(h *Hook) { h.StrictOutput = true },
	}
	// Adding a config field must also extend identity and its coverage.
	for field := range reflect.TypeFor[Hook]().Fields() {
		require.Contains(t, changes, field.Name)
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			other := base
			change(&other)
			exec := NewExecutor(&Config{SessionStart: []Hook{base, other, base, other}}, "", nil)
			assert.Equal(t, []Hook{base, other}, exec.hooksFor(EventSessionStart, ""))
		})
	}
}

func TestHookIdentityCollectionsAndBoundaries(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		first      Hook
		second     Hook
		duplicates bool
	}{
		{
			name: "nil and empty collections", duplicates: true,
			first:  Hook{Type: HookTypeBuiltin, Command: "count"},
			second: Hook{Type: HookTypeBuiltin, Command: "count", Args: []string{}, Env: map[string]string{}},
		},
		{
			name: "equal collection contents", duplicates: true,
			first:  Hook{Args: []string{"a", "b"}, Env: map[string]string{"a": "1", "b": "2"}},
			second: Hook{Args: []string{"a", "b"}, Env: map[string]string{"b": "2", "a": "1"}},
		},
		{name: "argument order", first: Hook{Args: []string{"a", "b"}}, second: Hook{Args: []string{"b", "a"}}},
		{name: "empty argument", first: Hook{}, second: Hook{Args: []string{""}}},
		{name: "empty environment value", first: Hook{}, second: Hook{Env: map[string]string{"PROFILE": ""}}},
		{name: "argument separator", first: Hook{Args: []string{"a\x00b"}}, second: Hook{Args: []string{"a", "b"}}},
		{name: "command separator", first: Hook{Command: "a\x00b"}, second: Hook{Command: "a", Args: []string{"b"}}},
		{name: "explicit timeout", first: Hook{}, second: Hook{Timeout: 60}},
		{name: "explicit error policy", first: Hook{}, second: Hook{OnError: "warn"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			exec := NewExecutor(&Config{SessionStart: []Hook{tc.first, tc.second}}, "", nil)
			want := []Hook{tc.first}
			if !tc.duplicates {
				want = append(want, tc.second)
			}
			assert.Equal(t, want, exec.hooksFor(EventSessionStart, ""))
		})
	}
}

func TestExecutorDedupIsPerDispatchAndKeepsFirstMatch(t *testing.T) {
	t.Parallel()

	first := Hook{Type: HookTypeBuiltin, Command: "append", Args: []string{"-first"}}
	second := Hook{Type: HookTypeBuiltin, Command: "append", Args: []string{"-second"}}
	registry := NewRegistry()
	require.NoError(t, registry.RegisterBuiltin("append", func(_ context.Context, in *Input, args []string) (*Output, error) {
		return &Output{HookSpecificOutput: &HookSpecificOutput{
			UpdatedInput: map[string]any{"cmd": in.ToolInput["cmd"].(string) + args[0]},
		}}, nil
	}))
	exec := NewExecutorWithRegistry(&Config{
		PreToolUse: []MatcherConfig{
			{Matcher: "other", Hooks: []Hook{second}},
			{Matcher: "shell", Hooks: []Hook{first}},
			{Matcher: "*", Hooks: []Hook{second, first}},
		},
		SessionStart: []Hook{first},
	}, "", nil, registry)
	assert.Equal(t, []Hook{first}, exec.hooksFor(EventSessionStart, ""))
	assert.Equal(t, []Hook{second, first}, exec.hooksFor(EventPreToolUse, "other"))
	for range 2 {
		result, err := exec.Dispatch(t.Context(), EventPreToolUse, &Input{ToolName: "shell", ToolInput: map[string]any{"cmd": "original"}})
		require.NoError(t, err)
		assert.Equal(t, "original-first-second", result.ModifiedInput["cmd"])
	}
}

func TestExecutorRunsDistinctCommandEnvironments(t *testing.T) {
	t.Parallel()

	firstDir, secondDir := t.TempDir(), t.TempDir()
	command := emitContextEnvPwdCmd("HOOK_PROFILE")
	exec := NewExecutor(&Config{SessionStart: []Hook{
		{Type: HookTypeCommand, Command: command, WorkingDir: firstDir, Env: map[string]string{"HOOK_PROFILE": "first"}},
		{Type: HookTypeCommand, Command: command, WorkingDir: secondDir, Env: map[string]string{"HOOK_PROFILE": "second"}},
	}}, "", nil)
	result, err := exec.Dispatch(t.Context(), EventSessionStart, &Input{})
	require.NoError(t, err)
	assert.Contains(t, result.AdditionalContext, "first:")
	assert.Contains(t, result.AdditionalContext, "second:")
}

func TestExecutorRunsDistinctBuiltinEnvironments(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	require.NoError(t, registry.RegisterBuiltin("env", func(ctx context.Context, _ *Input, _ []string) (*Output, error) {
		for _, entry := range EnvFromContext(ctx) {
			if strings.HasPrefix(entry, "HOOK_PROFILE=") {
				return NewAdditionalContextOutput(EventSessionStart, entry), nil
			}
		}
		return nil, nil
	}))
	first := Hook{Type: HookTypeBuiltin, Command: "env", Env: map[string]string{"HOOK_PROFILE": "first"}}
	second := first
	second.Env = maps.Clone(first.Env)
	second.Env["HOOK_PROFILE"] = "second"
	exec := NewExecutorWithRegistry(&Config{SessionStart: []Hook{first, second}}, "", nil, registry)
	result, err := exec.Dispatch(t.Context(), EventSessionStart, &Input{})
	require.NoError(t, err)
	assert.Equal(t, "HOOK_PROFILE=first\nHOOK_PROFILE=second", result.AdditionalContext)
}

func TestExecutorRunsDistinctModelHooks(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		first Hook
	}{
		{name: "prompt", first: Hook{Model: "test/judge", Prompt: "first", Schema: ShapePreToolUseDecision}},
		{name: "model", first: Hook{Model: "test/other", Prompt: "second", Schema: ShapePreToolUseDecision}},
		{name: "schema", first: Hook{Model: "test/judge", Prompt: "second"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			first := tc.first
			first.Type = HookTypeModel
			second := Hook{Type: HookTypeModel, Model: "test/judge", Prompt: "second", Schema: ShapePreToolUseDecision}
			client := &fakeClient{reply: `{"decision":"deny","reason":"policy"}`}
			registry := NewRegistry()
			registry.Register(HookTypeModel, NewModelFactory(client))
			exec := NewExecutorWithRegistry(&Config{PreToolUse: []MatcherConfig{{Hooks: []Hook{first, second, second}}}}, "", nil, registry)
			result, err := exec.Dispatch(t.Context(), EventPreToolUse, &Input{})
			require.NoError(t, err)
			assert.Equal(t, 2, client.calls)
			assert.Equal(t, "test/judge", client.gotModel)
			assert.Equal(t, "second", client.gotUser)
			assert.False(t, result.Allowed)
			assert.Equal(t, DecisionDeny, result.Decision)
		})
	}
}
