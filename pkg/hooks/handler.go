package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/docker/docker-agent/pkg/shellpath"
)

// Handler executes a single hook invocation. It is built by a
// [HandlerFactory] for one [Hook] and invoked at most once. The
// executor wraps ctx with the hook's timeout before calling Run, so
// handlers MUST NOT apply [Hook.GetTimeout] themselves.
type Handler interface {
	Run(ctx context.Context, input []byte) (HandlerResult, error)
}

// HandlerResult is the raw outcome of a [Handler.Run] call.
//
// Handlers can speak to the executor in either of two ways:
//   - Process protocol: leave Output nil; write JSON (or plain text) to
//     Stdout, signal blocking with ExitCode == 2, etc. The executor
//     parses Stdout as JSON when ExitCode == 0 and it begins with '{'.
//   - Direct protocol: set Output to a pre-parsed [Output] to skip the
//     JSON round-trip; ExitCode should stay 0 and Stdout/Stderr can be
//     left empty.
type HandlerResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Output   *Output
}

// HandlerEnv carries per-executor context exposed to factories.
type HandlerEnv struct {
	WorkingDir string
	Env        []string
}

// HandlerFactory builds a [Handler] for a single hook invocation.
// Factories validate the hook (e.g. non-empty [Hook.Command]) and
// return an error if it isn't runnable.
type HandlerFactory func(env HandlerEnv, hook Hook) (Handler, error)

// BuiltinFunc is the signature of an in-process hook handler. It
// receives the parsed [Input] (no JSON unmarshaling) plus per-hook
// [Hook.Args], and returns a parsed [Output]. Returning a nil Output is
// a successful no-op.
type BuiltinFunc func(ctx context.Context, in *Input, args []string) (*Output, error)

// Registry maps [HookType] to [HandlerFactory], plus a name → [BuiltinFunc]
// table for [HookTypeBuiltin]. Safe for concurrent use.
type Registry struct {
	mu        sync.RWMutex
	factories map[HookType]HandlerFactory
	builtins  map[string]BuiltinFunc
}

// NewRegistry returns a registry pre-populated with [HookTypeCommand]
// (shell command hooks) and [HookTypeBuiltin] (in-process functions).
func NewRegistry() *Registry {
	r := &Registry{
		factories: map[HookType]HandlerFactory{},
		builtins:  map[string]BuiltinFunc{},
	}
	r.Register(HookTypeCommand, newCommandFactory())
	r.Register(HookTypeBuiltin, r.builtinFactory)
	return r
}

// Register associates a factory with a hook type, replacing any prior one.
func (r *Registry) Register(t HookType, f HandlerFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.factories[t] = f
}

// Lookup returns the factory registered for t, or (nil, false).
func (r *Registry) Lookup(t HookType) (HandlerFactory, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	f, ok := r.factories[t]
	return f, ok
}

// RegisterBuiltin makes fn callable as `{type: builtin, command: name}`.
// Empty name or nil fn are rejected.
func (r *Registry) RegisterBuiltin(name string, fn BuiltinFunc) error {
	if name == "" {
		return errors.New("builtin hook name must not be empty")
	}
	if fn == nil {
		return errors.New("builtin hook function must not be nil")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.builtins[name] = fn
	return nil
}

// LookupBuiltin returns the function registered as name, or (nil, false).
func (r *Registry) LookupBuiltin(name string) (BuiltinFunc, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	fn, ok := r.builtins[name]
	return fn, ok
}

// DefaultRegistry is the process-wide registry used by [NewExecutor].
// Callers needing runtime-owned builtins should construct a private
// registry rather than mutating this one.
var DefaultRegistry = NewRegistry()

// newCommandFactory resolves the OS shell once at factory-build time so
// per-hook invocations don't pay the shell-detection cost.
func newCommandFactory() HandlerFactory {
	shell, shellArgs := shellpath.DetectShell()
	return func(env HandlerEnv, hook Hook) (Handler, error) {
		if hook.Command == "" {
			return nil, errors.New("command hook requires a non-empty command")
		}
		return &commandHandler{
			workingDir: hookWorkingDir(env.WorkingDir, hook.WorkingDir),
			env:        hookEnv(env.Env, hook.Env),
			shell:      shell,
			shellArgs:  shellArgs,
			command:    hook.Command,
		}, nil
	}
}

func hookWorkingDir(base, override string) string {
	if override == "" || filepath.IsAbs(override) {
		return override
	}
	if base == "" {
		return override
	}
	return filepath.Join(base, override)
}

func hookEnv(base []string, overrides map[string]string) []string {
	if len(overrides) == 0 {
		return base
	}
	env := slices.Clone(base)
	if len(env) == 0 {
		env = os.Environ()
	}

	// Map each existing key to its position so overrides can replace in place.
	index := make(map[string]int, len(env))
	for i, entry := range env {
		if key, _, ok := strings.Cut(entry, "="); ok {
			index[key] = i
		}
	}

	for key, value := range overrides {
		entry := key + "=" + value
		if i, ok := index[key]; ok {
			env[i] = entry
		} else {
			env = append(env, entry)
		}
	}
	return env
}

// commandHandler runs a hook by exec'ing its command under a shell.
type commandHandler struct {
	workingDir string
	env        []string
	shell      string
	shellArgs  []string
	command    string
}

func (h *commandHandler) Run(ctx context.Context, input []byte) (HandlerResult, error) {
	cmd := exec.CommandContext(ctx, h.shell, append(h.shellArgs, h.command)...)
	cmd.Dir = h.workingDir
	cmd.Env = h.env
	cmd.Stdin = bytes.NewReader(input)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	res := HandlerResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		// ExitError → structured exit code; anything else (binary
		// missing, ...) bubbles up so PreToolUse fails closed.
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			res.ExitCode = exitErr.ExitCode()
			return res, nil
		}
		res.ExitCode = -1
		return res, err
	}
	return res, nil
}

// builtinFactory resolves Hook.Command in the registry's builtin table
// and returns a [Handler] that bridges the JSON-on-stdin protocol to a
// typed [BuiltinFunc].
func (r *Registry) builtinFactory(_ HandlerEnv, hook Hook) (Handler, error) {
	if hook.Command == "" {
		return nil, errors.New("builtin hook requires a name in command")
	}
	fn, ok := r.LookupBuiltin(hook.Command)
	if !ok {
		return nil, fmt.Errorf("no builtin hook registered as %q", hook.Command)
	}
	return &builtinHandler{fn: fn, args: hook.Args}, nil
}

type builtinHandler struct {
	fn   BuiltinFunc
	args []string
}

func (h *builtinHandler) Run(ctx context.Context, input []byte) (HandlerResult, error) {
	var in Input
	if err := json.Unmarshal(input, &in); err != nil {
		return HandlerResult{ExitCode: -1}, fmt.Errorf("decode hook input: %w", err)
	}
	out, err := h.fn(ctx, &in, h.args)
	if err != nil {
		return HandlerResult{ExitCode: -1}, err
	}
	return HandlerResult{Output: out}, nil
}
