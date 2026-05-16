package hooks

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRegisterBuiltinValidation pins the input contract: empty names and
// nil functions are rejected, valid pairs round-trip through LookupBuiltin.
func TestRegisterBuiltinValidation(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()

	require.Error(t, registry.RegisterBuiltin("", func(context.Context, *Input, []string) (*Output, error) { return nil, nil }))
	require.Error(t, registry.RegisterBuiltin("nil-fn", nil))

	require.NoError(t, registry.RegisterBuiltin("echo", func(context.Context, *Input, []string) (*Output, error) {
		return &Output{}, nil
	}))

	fn, ok := registry.LookupBuiltin("echo")
	require.True(t, ok)
	require.NotNil(t, fn)

	_, ok = registry.LookupBuiltin("never-registered")
	require.False(t, ok)
}

// TestExecutorDispatchesBuiltinHook is the end-to-end happy path: a Go
// function is registered on a private Registry, referenced from a hook
// with {type: builtin, command: <name>}, and its returned Output drives
// the aggregated Result. The handler also sees the typed Input directly,
// without having to unmarshal JSON itself.
func TestExecutorDispatchesBuiltinHook(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()

	var (
		called   bool
		seenTool string
	)
	require.NoError(t, registry.RegisterBuiltin("deny", func(_ context.Context, in *Input, _ []string) (*Output, error) {
		called = true
		seenTool = in.ToolName
		return &Output{
			Decision: "block",
			Reason:   "denied by builtin hook",
		}, nil
	}))

	config := &Config{
		PreToolUse: []MatcherConfig{
			{
				Matcher: "*",
				Hooks: []Hook{
					{Type: HookTypeBuiltin, Command: "deny", Timeout: 5},
				},
			},
		},
	}

	exec := NewExecutorWithRegistry(config, t.TempDir(), nil, registry)
	result, err := exec.Dispatch(t.Context(), EventPreToolUse, &Input{
		SessionID: "test-session",
		ToolName:  "shell",
		ToolUseID: "test-id",
	})
	require.NoError(t, err)

	assert.True(t, called)
	assert.Equal(t, "shell", seenTool)
	assert.False(t, result.Allowed)
	assert.Contains(t, result.Message, "denied by builtin hook")
}

// TestBuiltinHookUnknownNameIsRejected ensures that referencing an
// unregistered builtin from a hook surfaces as a hook execution error.
// For PreToolUse this maps to fail-closed (deny), matching how the
// existing "unsupported hook type" path behaves.
func TestBuiltinHookUnknownNameIsRejected(t *testing.T) {
	t.Parallel()

	config := &Config{
		PreToolUse: []MatcherConfig{
			{
				Matcher: "*",
				Hooks: []Hook{
					{Type: HookTypeBuiltin, Command: "never-registered", Timeout: 5},
				},
			},
		},
	}

	exec := NewExecutorWithRegistry(config, t.TempDir(), nil, NewRegistry())
	result, err := exec.Dispatch(t.Context(), EventPreToolUse, &Input{
		SessionID: "test-session",
		ToolName:  "shell",
		ToolUseID: "test-id",
	})
	require.NoError(t, err)
	assert.False(t, result.Allowed)
	assert.Contains(t, result.Message, "no builtin hook registered")
}

// TestBuiltinHookEmptyNameIsRejected ensures the factory rejects a hook
// that uses HookTypeBuiltin without naming a function.
func TestBuiltinHookEmptyNameIsRejected(t *testing.T) {
	t.Parallel()

	config := &Config{
		PreToolUse: []MatcherConfig{
			{
				Matcher: "*",
				Hooks: []Hook{
					{Type: HookTypeBuiltin, Command: "", Timeout: 5},
				},
			},
		},
	}

	exec := NewExecutorWithRegistry(config, t.TempDir(), nil, NewRegistry())
	result, err := exec.Dispatch(t.Context(), EventPreToolUse, &Input{
		SessionID: "test-session",
		ToolName:  "shell",
		ToolUseID: "test-id",
	})
	require.NoError(t, err)
	assert.False(t, result.Allowed)
	assert.Contains(t, result.Message, "builtin hook requires a name")
}

// TestBuiltinHookErrorFailsClosed documents that an error returned by the
// builtin function is treated identically to a command hook spawn failure:
// for PreToolUse it denies the call.
func TestBuiltinHookErrorFailsClosed(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	require.NoError(t, registry.RegisterBuiltin("boom", func(context.Context, *Input, []string) (*Output, error) {
		return nil, errors.New("kaboom")
	}))

	config := &Config{
		PreToolUse: []MatcherConfig{
			{
				Matcher: "*",
				Hooks: []Hook{
					{Type: HookTypeBuiltin, Command: "boom", Timeout: 5},
				},
			},
		},
	}

	exec := NewExecutorWithRegistry(config, t.TempDir(), nil, registry)
	result, err := exec.Dispatch(t.Context(), EventPreToolUse, &Input{
		SessionID: "test-session",
		ToolName:  "shell",
		ToolUseID: "test-id",
	})
	require.NoError(t, err)
	assert.False(t, result.Allowed)
	assert.Equal(t, -1, result.ExitCode)
}
