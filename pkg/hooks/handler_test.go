package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
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
	cp := slices.Clone(input)
	if len(cp) == 0 {
		cp = nil
	}
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
		Command:    emitContextEnvPwdCmd("HOOK_VALUE"),
		Env:        map[string]string{"HOOK_VALUE": "from-hook"},
		WorkingDir: "scripts",
	}}}, baseDir, os.Environ())

	result, err := exec.Dispatch(t.Context(), EventSessionStart, &Input{SessionID: "s"})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(result.AdditionalContext, "from-hook:"), result.AdditionalContext)
	assertSamePath(t, scriptsDir, strings.TrimPrefix(result.AdditionalContext, "from-hook:"))
}

// TestCommandHookExpandsEnvRefs pins the ${env.X} expansion contract for
// hook fields (issue #2615): working_dir accepts ${env.X} (and ~/$X via
// path.ExpandPath), env values expand only the plain ${env.X} form, and
// shell-style $X stays literal so values containing $ are not mangled.
func TestCommandHookExpandsEnvRefs(t *testing.T) {
	baseDir := t.TempDir()
	scriptsDir := filepath.Join(baseDir, "scripts")
	require.NoError(t, os.Mkdir(scriptsDir, 0o755))

	t.Setenv("HOOK_TEST_DIR", "scripts")
	t.Setenv("HOOK_TEST_TOKEN", "tok")

	exec := NewExecutor(&Config{SessionStart: []Hook{{
		Type:    HookTypeCommand,
		Command: emitContextEnvPwdCmd("HOOK_VALUE", "HOOK_LITERAL"),
		Env: map[string]string{
			"HOOK_VALUE":   "v-${env.HOOK_TEST_TOKEN}",
			"HOOK_LITERAL": "pa$$word",
		},
		WorkingDir: "${env.HOOK_TEST_DIR}",
	}}}, baseDir, os.Environ())

	result, err := exec.Dispatch(t.Context(), EventSessionStart, &Input{SessionID: "s"})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(result.AdditionalContext, "v-tok:pa$$word:"), result.AdditionalContext)
	assertSamePath(t, scriptsDir, strings.TrimPrefix(result.AdditionalContext, "v-tok:pa$$word:"))
}

// TestCommandHookDefaultsToExecutorWorkingDir pins that a hook WITHOUT a
// working_dir override runs in the executor's working directory rather
// than inheriting the process cwd. This matters for executors that run
// before the process has chdir'd into their working dir — e.g. the
// CLI-dispatched worktree_create event, whose working dir is the freshly
// created worktree.
func TestCommandHookDefaultsToExecutorWorkingDir(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()

	exec := NewExecutor(&Config{SessionStart: []Hook{{
		Type:    HookTypeCommand,
		Command: emitContextEnvPwdCmd(),
	}}}, workDir, os.Environ())

	result, err := exec.Dispatch(t.Context(), EventSessionStart, &Input{SessionID: "s"})
	require.NoError(t, err)
	assertSamePath(t, workDir, strings.TrimSpace(result.AdditionalContext))
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

func TestRegistryClone(t *testing.T) {
	t.Parallel()
	original := NewRegistry()
	require.NoError(t, original.RegisterBuiltin("where", func(_ context.Context, in *Input, _ []string) (*Output, error) {
		return &Output{SystemMessage: in.Cwd}, nil
	}))
	cloned := original.Clone()
	require.NoError(t, cloned.RegisterBuiltin("where", func(context.Context, *Input, []string) (*Output, error) {
		return &Output{SystemMessage: "changed"}, nil
	}))
	first, ok := original.LookupBuiltin("where")
	require.True(t, ok)
	out, err := first(t.Context(), &Input{Cwd: "original"}, nil)
	require.NoError(t, err)
	assert.Equal(t, "original", out.SystemMessage)
	second, ok := cloned.LookupBuiltin("where")
	require.True(t, ok)
	out, err = second(t.Context(), &Input{}, nil)
	require.NoError(t, err)
	assert.Equal(t, "changed", out.SystemMessage)
	factory, ok := cloned.Lookup(HookTypeBuiltin)
	require.True(t, ok)
	handler, err := factory(HandlerEnv{}, Hook{Type: HookTypeBuiltin, Command: "where"})
	require.NoError(t, err)
	result, err := handler.Run(t.Context(), []byte(`{}`))
	require.NoError(t, err)
	assert.Equal(t, "changed", result.Output.SystemMessage)
}

func TestRegistryClonePreservesBuiltinFactoryOverride(t *testing.T) {
	t.Parallel()
	original := NewRegistry()
	invoked := false
	original.Register(HookTypeBuiltin, func(HandlerEnv, Hook) (Handler, error) {
		invoked = true
		return nil, errors.New("custom policy rejected hook")
	})
	clone := original.Clone()
	factory, ok := clone.Lookup(HookTypeBuiltin)
	require.True(t, ok)
	_, err := factory(HandlerEnv{}, Hook{})
	require.ErrorContains(t, err, "custom policy")
	assert.True(t, invoked)
}
