package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingHandler is an in-process [Handler] used to prove that the
// executor dispatches to whatever factory the [Registry] returns, not just
// the built-in command handler. It captures the JSON input it received and
// returns a pre-parsed [Output] so the executor's "honor
// HandlerResult.Output as-is" path is also exercised.
type recordingHandler struct {
	calls atomic.Int32
	input atomic.Value // []byte
	out   *Output
}

func (h *recordingHandler) Run(_ context.Context, input []byte) (HandlerResult, error) {
	h.calls.Add(1)
	cp := append([]byte(nil), input...)
	h.input.Store(cp)
	return HandlerResult{Output: h.out}, nil
}

func (h *recordingHandler) capturedInput() []byte {
	v, _ := h.input.Load().([]byte)
	return v
}

// TestExecutorDispatchesToCustomHandler shows the smallest end-to-end use
// of the new pluggability: a custom HookType backed by an in-process Go
// Handler runs through the same executor pipeline as a "command" hook,
// and its pre-parsed Output (a deny verdict here) drives the aggregated
// Result.
func TestExecutorDispatchesToCustomHandler(t *testing.T) {
	t.Parallel()

	const customType HookType = "builtin-test"

	rec := &recordingHandler{
		out: &Output{
			Decision: "block",
			Reason:   "denied by builtin handler",
		},
	}

	registry := NewRegistry()
	registry.Register(customType, func(_ HandlerEnv, _ Hook) (Handler, error) {
		return rec, nil
	})

	config := &Config{
		PreToolUse: []MatcherConfig{
			{
				Matcher: "*",
				Hooks: []Hook{
					{Type: customType, Timeout: 5},
				},
			},
		},
	}

	exec := NewExecutorWithRegistry(config, t.TempDir(), nil, registry)
	input := &Input{
		SessionID: "test-session",
		ToolName:  "shell",
		ToolUseID: "test-id",
	}

	result, err := exec.Dispatch(t.Context(), EventPreToolUse, input)
	require.NoError(t, err)

	// The custom handler ran and saw a properly serialized Input on stdin.
	assert.Equal(t, int32(1), rec.calls.Load())
	var got Input
	require.NoError(t, json.Unmarshal(rec.capturedInput(), &got))
	assert.Equal(t, EventPreToolUse, got.HookEventName)
	assert.Equal(t, "shell", got.ToolName)

	// The pre-parsed Output drove the aggregated Result, so the call is
	// denied with the handler-supplied reason.
	assert.False(t, result.Allowed)
	assert.Contains(t, result.Message, "denied by builtin handler")
}

// TestExecutorUnregisteredTypeIsRejected ensures the registry is the only
// way to plug in a handler: an unknown HookType is surfaced as a hook
// execution error, which (because PreToolUse is a security boundary)
// denies the tool call.
func TestExecutorUnregisteredTypeIsRejected(t *testing.T) {
	t.Parallel()

	config := &Config{
		PreToolUse: []MatcherConfig{
			{
				Matcher: "*",
				Hooks: []Hook{
					{Type: HookType("nope"), Timeout: 5},
				},
			},
		},
	}

	exec := NewExecutorWithRegistry(config, t.TempDir(), nil, NewRegistry())
	input := &Input{
		SessionID: "test-session",
		ToolName:  "shell",
		ToolUseID: "test-id",
	}

	result, err := exec.Dispatch(t.Context(), EventPreToolUse, input)
	require.NoError(t, err)
	assert.False(t, result.Allowed)
	assert.Contains(t, result.Message, "unsupported hook type")
}

// TestExecutorHandlerErrorDeniesPreToolUse documents the contract that any
// handler error (not just a non-zero exit) flows into the existing
// fail-closed behavior for PreToolUse.
func TestExecutorHandlerErrorDeniesPreToolUse(t *testing.T) {
	t.Parallel()

	const customType HookType = "always-fails"

	registry := NewRegistry()
	registry.Register(customType, func(_ HandlerEnv, _ Hook) (Handler, error) {
		return handlerFunc(func(context.Context, []byte) (HandlerResult, error) {
			return HandlerResult{ExitCode: -1}, errors.New("kaboom")
		}), nil
	})

	config := &Config{
		PreToolUse: []MatcherConfig{
			{
				Matcher: "*",
				Hooks: []Hook{
					{Type: customType, Timeout: 5},
				},
			},
		},
	}

	exec := NewExecutorWithRegistry(config, t.TempDir(), nil, registry)
	input := &Input{
		SessionID: "test-session",
		ToolName:  "shell",
		ToolUseID: "test-id",
	}

	result, err := exec.Dispatch(t.Context(), EventPreToolUse, input)
	require.NoError(t, err)
	assert.False(t, result.Allowed)
	assert.Equal(t, -1, result.ExitCode)
}

// TestExecutorHandlerErrorPreservesStderr pins the contract that when a
// handler returns an error, the diagnostic stderr it captured before
// failing survives all the way to [Result.Stderr]. aggregate routes
// that field into the user-visible PreToolUse fail-closed message; if
// runHook ever drops it on the floor (as it briefly did during the
// HandlerResult-embedding refactor) PreToolUse denials would lose
// their explanatory text.
func TestExecutorHandlerErrorPreservesStderr(t *testing.T) {
	t.Parallel()

	const customType HookType = "errors-with-stderr"
	const diagnostic = "BOOM: subprocess crashed at line 42"

	registry := NewRegistry()
	registry.Register(customType, func(_ HandlerEnv, _ Hook) (Handler, error) {
		return handlerFunc(func(context.Context, []byte) (HandlerResult, error) {
			// Mirrors what commandHandler does on a spawn failure: it
			// captured stderr, then surfaces an exec-level error.
			return HandlerResult{Stderr: diagnostic, ExitCode: -1}, errors.New("spawn failed")
		}), nil
	})

	config := &Config{
		PreToolUse: []MatcherConfig{
			{Matcher: "*", Hooks: []Hook{{Type: customType, Timeout: 5}}},
		},
	}

	exec := NewExecutorWithRegistry(config, t.TempDir(), nil, registry)
	result, err := exec.Dispatch(t.Context(), EventPreToolUse, &Input{
		SessionID: "s", ToolName: "shell", ToolUseID: "t",
	})
	require.NoError(t, err)

	assert.False(t, result.Allowed)
	assert.Equal(t, -1, result.ExitCode)
	assert.Equal(t, diagnostic, result.Stderr,
		"handler-captured stderr must survive the err != nil normalization in runHook")
}

// handlerFunc adapts a function value into a [Handler] for terse tests.
type handlerFunc func(context.Context, []byte) (HandlerResult, error)

func (f handlerFunc) Run(ctx context.Context, input []byte) (HandlerResult, error) {
	return f(ctx, input)
}

func TestCommandHookUsesPerHookEnvAndWorkingDir(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()
	scriptsDir := filepath.Join(baseDir, "scripts")
	require.NoError(t, os.Mkdir(scriptsDir, 0o755))

	exec := NewExecutor(&Config{SessionStart: []Hook{{
		Type:       HookTypeCommand,
		Command:    `printf '{"hook_specific_output":{"additional_context":"%s:%s"}}' "$HOOK_VALUE" "$(pwd)"`,
		Env:        map[string]string{"HOOK_VALUE": "from-hook"},
		WorkingDir: "scripts",
	}}}, baseDir, os.Environ())

	result, err := exec.Dispatch(t.Context(), EventSessionStart, &Input{SessionID: "s"})
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(result.AdditionalContext, "from-hook:"), result.AdditionalContext)
	assert.True(t, strings.HasSuffix(result.AdditionalContext, "/scripts"), result.AdditionalContext)
}

func TestHookOnErrorBlockCanDenyNonFailClosedEvent(t *testing.T) {
	t.Parallel()

	exec := NewExecutor(&Config{PostToolUse: []MatcherConfig{{
		Matcher: "*",
		Hooks: []Hook{{
			Type:    HookTypeBuiltin,
			Command: "missing",
			Name:    "missing builtin",
			OnError: string(ErrorPolicyBlock),
		}},
	}}}, t.TempDir(), nil)

	result, err := exec.Dispatch(t.Context(), EventPostToolUse, &Input{SessionID: "s", ToolName: "shell"})
	require.NoError(t, err)
	assert.False(t, result.Allowed)
	assert.Contains(t, result.Message, "no builtin hook registered")
}
