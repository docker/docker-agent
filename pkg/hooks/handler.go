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

	"github.com/docker/docker-agent/pkg/concurrent"
	"github.com/docker/docker-agent/pkg/path"
	"github.com/docker/docker-agent/pkg/shellpath"
	"github.com/docker/docker-agent/pkg/telemetry/genai"
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

// hookEnvKey is the context key under which a builtin hook's resolved
// environment is carried to the [BuiltinFunc].
type hookEnvKey struct{}

// withHookEnv returns a context carrying env. A nil env is stored as-is
// so [EnvFromContext] reports "inherit the process environment".
func withHookEnv(ctx context.Context, env []string) context.Context {
	return context.WithValue(ctx, hookEnvKey{}, env)
}

// EnvFromContext returns the environment a builtin hook should use for
// any subprocess it spawns, as "KEY=value" entries. It merges the
// executor's environment with the hook's per-hook env overrides. A nil
// result means "inherit the current process environment" (the default
// for hooks without an env override), matching exec.Cmd semantics.
//
// Builtins that don't shell out can ignore it. Those that do should
// pass it to exec.Cmd.Env (falling back to os.Environ() on nil only if
// they must materialize a concrete slice).
func EnvFromContext(ctx context.Context) []string {
	env, _ := ctx.Value(hookEnvKey{}).([]string)
	return env
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
	factories concurrent.Map[HookType, HandlerFactory]
	builtins  concurrent.Map[string, BuiltinFunc]
}

// NewRegistry returns a registry pre-populated with [HookTypeCommand]
// (shell command hooks) and [HookTypeBuiltin] (in-process functions).
func NewRegistry() *Registry {
	r := &Registry{}
	r.Register(HookTypeCommand, newCommandFactory())
	r.Register(HookTypeBuiltin, r.builtinFactory)
	return r
}

// Register associates a factory with a hook type, replacing any prior one.
func (r *Registry) Register(t HookType, f HandlerFactory) {
	r.factories.Store(t, f)
}

// Lookup returns the factory registered for t, or (nil, false).
func (r *Registry) Lookup(t HookType) (HandlerFactory, bool) {
	return r.factories.Load(t)
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
	r.builtins.Store(name, fn)
	return nil
}

// LookupBuiltin returns the function registered as name, or (nil, false).
func (r *Registry) LookupBuiltin(name string) (BuiltinFunc, bool) {
	return r.builtins.Load(name)
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

// hookWorkingDir resolves the directory a command hook runs in. An
// absolute override wins; a relative override is joined onto the
// executor's working directory; with no override the executor's working
// directory is used. Falling back to the executor's working directory
// (rather than "", which would inherit the process cwd) matters when the
// executor runs before the process has chdir'd into it — e.g. the
// CLI-dispatched worktree_create event, whose working dir is the freshly
// created worktree.
//
// The override accepts ~, $VAR, ${VAR} and ${env.VAR} like every other
// working_dir field (issue #2615); expansion runs before the absolute
// check so ~/... overrides win.
func hookWorkingDir(base, override string) string {
	override = path.ExpandWorkingDir("hook working_dir", override)
	if filepath.IsAbs(override) {
		return override
	}
	if override == "" {
		return base
	}
	if base == "" {
		return override
	}
	return filepath.Join(base, override)
}

// hookEnv layers the hook's env overrides onto the executor's env. Only
// the plain ${env.X} form is expanded (against the OS environment) in
// override values; $X and ${X}
// stay literal because env values may legitimately contain $ (issue #2615).
//
// Expansion is per-invocation, not config-load: factories run inside
// [Executor.runHook] each time a hook fires, so ${env.X} reflects the OS
// environment at fire time (matching script_shell's per-call expansion).
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
		entry := key + "=" + path.ExpandEnvRefs(value)
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
	// Expand nil to os.Environ() so the child inherits the parent env
	// (matching the pre-OTel cmd.Env=h.env=nil behaviour), and copy
	// into a fresh backing array so concurrent hooks don't race on a
	// shared slice when adding the trace-context vars.
	base := h.env
	if base == nil {
		base = os.Environ()
	}
	traceEnv := genai.InjectTraceContextEnv(ctx)
	// A nil Cmd.Env would inherit the process environment.
	envCopy := make([]string, 0, len(base)+len(traceEnv)) //rubocop:disable Lint/SlicesConcat
	envCopy = append(envCopy, base...)
	envCopy = append(envCopy, traceEnv...)
	cmd.Env = envCopy
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
//
// The factory resolves the same working_dir and env overrides honored
// by command hooks: working_dir repoints the [Input.Cwd] every builtin
// keys off (the git/listing builtins shell out or stat relative to it),
// and env is exposed via [EnvFromContext] so builtins that exec a
// subprocess can run it with per-hook variables. Both default to the
// executor's values when the hook leaves them unset, preserving prior
// behavior.
func (r *Registry) builtinFactory(env HandlerEnv, hook Hook) (Handler, error) {
	if hook.Command == "" {
		return nil, errors.New("builtin hook requires a name in command")
	}
	fn, ok := r.LookupBuiltin(hook.Command)
	if !ok {
		return nil, fmt.Errorf("no builtin hook registered as %q", hook.Command)
	}
	return &builtinHandler{
		fn:         fn,
		args:       hook.Args,
		workingDir: hook.WorkingDir,
		baseDir:    env.WorkingDir,
		env:        hookEnv(env.Env, hook.Env),
	}, nil
}

type builtinHandler struct {
	fn   BuiltinFunc
	args []string
	// workingDir is the hook's working_dir override (possibly relative);
	// baseDir is the executor's working directory it resolves against.
	workingDir string
	baseDir    string
	env        []string
}

func (h *builtinHandler) Run(ctx context.Context, input []byte) (HandlerResult, error) {
	var in Input
	if err := json.Unmarshal(input, &in); err != nil {
		return HandlerResult{ExitCode: -1}, fmt.Errorf("decode hook input: %w", err)
	}
	// A working_dir override repoints Input.Cwd, the directory every
	// builtin keys off. With no override Input.Cwd is left as-is so
	// callers that supply a cwd (e.g. the worktree_create event) keep it.
	if h.workingDir != "" {
		in.Cwd = hookWorkingDir(h.baseDir, h.workingDir)
	}
	ctx = withHookEnv(ctx, h.env)
	out, err := h.fn(ctx, &in, h.args)
	if err != nil {
		return HandlerResult{ExitCode: -1}, err
	}
	return HandlerResult{Output: out}, nil
}
