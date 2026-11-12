package acp

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/coder/acp-go-sdk"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/tools"
	envtool "github.com/docker/docker-agent/pkg/tools/builtin/environment"
	"github.com/docker/docker-agent/pkg/tools/builtin/shell"
)

type terminalToolset struct {
	base        *shell.ToolSet
	workingDir  string
	env         []acp.EnvVariable
	sudoAskpass bool
}

func newTerminalToolset(ctx context.Context, cfg latest.Toolset, rc *config.RuntimeConfig) (tools.ToolSet, error) {
	for name := range cfg.Env {
		if name == "" || strings.ContainsAny(name, "=\x00") {
			return nil, errors.New("invalid terminal environment variable name")
		}
	}
	expanded, err := environment.ExpandAll(ctx, environment.ToValues(cfg.Env), rc.EnvProvider())
	if err != nil {
		return nil, fmt.Errorf("expanding terminal environment: %w", err)
	}
	var env []acp.EnvVariable
	for _, entry := range expanded {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || name == "" || strings.ContainsRune(entry, 0) {
			return nil, errors.New("invalid terminal environment variable")
		}
		env = append(env, acp.EnvVariable{Name: name, Value: value})
	}
	wd, err := filepath.Abs(rc.WorkingDir)
	if err != nil {
		return nil, err
	}
	return &terminalToolset{base: shell.New(nil, rc), workingDir: wd, env: env, sudoAskpass: cfg.SudoAskpass != nil && *cfg.SudoAskpass}, nil
}

func (t *terminalToolset) Instructions() string {
	program, _ := terminalInterpreter()
	return fmt.Sprintf(`## ACP Client Shell

- shell runs in the ACP client's terminal using %s, not the agent host's detected shell.
- Client and agent must use the same platform and workspace paths. Host environment-info and hooks describe the agent host, not the client shell.
- Each call uses a fresh shell. Only configured environment overrides are sent; the client owns its inherited environment.
- Default timeout is 30s; set timeout for longer commands. Output retains at most the last 64 KiB.
- Client failures never run the command locally. Script, Git, and post-edit hooks still execute locally.`, program)
}

func (t *terminalToolset) Tools(ctx context.Context) ([]tools.Tool, error) {
	definitions, err := t.base.Tools(ctx)
	if err != nil {
		return nil, err
	}
	for i := range definitions {
		if definitions[i].Name == shell.ToolNameShell {
			program, _ := terminalInterpreter()
			definitions[i].Description = "Execute a shell command in the ACP client's terminal using " + program + ". Client and agent must have matching platforms and workspace paths; no local fallback."
			definitions[i].Handler = tools.NewRuntimeHandler(t.run)
		}
	}
	return definitions, nil
}

func (t *terminalToolset) run(ctx context.Context, args shell.RunShellArgs, _ tools.Runtime) (result *tools.ToolCallResult, retErr error) {
	if strings.TrimSpace(args.Cmd) == "" {
		return tools.ResultError(`Error: missing or empty "cmd" parameter.`), nil
	}
	if t.sudoAskpass {
		return tools.ResultError("sudo_askpass is not supported by ACP client terminals"), nil
	}
	owner, _ := ctx.Value(terminalOwnerKey{}).(*terminalManager)
	if owner == nil {
		return tools.ResultError("ACP terminal session owner not available"), nil
	}
	timeout := 30 * time.Second
	if args.Timeout > 0 {
		if int64(args.Timeout) > math.MaxInt64/int64(time.Second) {
			return tools.ResultError("invalid terminal timeout"), nil
		}
		timeout = time.Duration(args.Timeout) * time.Second
	}
	cwd := args.Cwd
	if cwd == "" || cwd == "." {
		cwd = t.workingDir
	} else if !filepath.IsAbs(cwd) {
		cwd = filepath.Join(t.workingDir, cwd)
	}
	if !filepath.IsAbs(cwd) || strings.ContainsRune(cwd, 0) {
		return tools.ResultError("terminal cwd must be an absolute path"), nil
	}
	opCtx, op, err := owner.acquire(ctx)
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	defer op.finish()
	defer func() {
		if err := op.release(ctx); err != nil {
			result = tools.ResultError("ACP terminal release failed; command may have run and cleanup remains unresolved")
		} else if ctx.Err() != nil || opCtx.Err() != nil {
			// Output transforms can be discarded on cancellation; never return raw partial output.
			result = tools.ResultSuccess("Command cancelled")
		}
	}()
	runCtx, cancel := context.WithTimeout(opCtx, timeout)
	defer cancel()
	command, prefix := terminalInterpreter()
	if command == "" {
		return tools.ResultError("trusted client terminal interpreter is unavailable"), nil
	}
	err = op.create(runCtx, acp.CreateTerminalRequest{SessionId: owner.sid, Command: command, Args: append(prefix, args.Cmd), Cwd: &cwd, Env: t.env, OutputByteLimit: new(terminalOutputLimit)})
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	var exit acp.WaitForTerminalExitResponse
	if runCtx.Err() == nil {
		exit, err = owner.conn.WaitForTerminalExit(runCtx, acp.WaitForTerminalExitRequest{Meta: traceMeta(runCtx, nil), SessionId: owner.sid, TerminalId: op.id})
	} else {
		err = runCtx.Err()
	}
	failedWait := err != nil
	executionErr := runCtx.Err()
	cancel()
	var killErr error
	if failedWait {
		killErr = op.kill(ctx)
	}
	outputCtx, stopOutput := context.WithTimeout(context.WithoutCancel(ctx), terminalCleanupBudget)
	defer stopOutput()
	output, outputErr := owner.conn.TerminalOutput(outputCtx, acp.TerminalOutputRequest{Meta: traceMeta(outputCtx, nil), SessionId: owner.sid, TerminalId: op.id})
	if outputErr != nil {
		return tools.ResultError("Error reading ACP terminal output: " + outputErr.Error()), nil
	}
	text := terminalOutputTail(output.Output, output.Truncated)
	switch {
	case executionErr != nil:
		if ctx.Err() != nil || opCtx.Err() != nil {
			text = "Command cancelled"
		} else {
			text = fmt.Sprintf("Command timed out after %v\nOutput: %s", timeout, text)
		}
	case failedWait:
		return tools.ResultError("Error waiting for ACP terminal: " + err.Error() + "\nOutput: " + text), nil
	case exit.Signal != nil:
		text = "Error executing command: signal " + *exit.Signal + "\nOutput: " + text
	case exit.ExitCode == nil:
		return tools.ResultError("ACP terminal returned no exit status\nOutput: " + text), nil
	case *exit.ExitCode != 0:
		text = fmt.Sprintf("Error executing command: exit status %d\nOutput: %s", *exit.ExitCode, text)
	}
	if killErr != nil {
		text += "\nTerminal kill failed; release will also terminate the command."
	}
	return tools.ResultSuccess(cmp.Or(strings.TrimSpace(text), "<no output>")), nil
}

func terminalOutputTail(text string, truncated bool) string {
	if len(text) > terminalOutputLimit {
		start := len(text) - terminalOutputLimit
		for start < len(text) && !utf8.RuneStart(text[start]) {
			start++
		}
		text = text[start:]
		truncated = true
	}
	if truncated {
		return "[Terminal output truncated; showing retained tail.]\n" + text
	}
	return text
}

type terminalEnvironment struct{ envtool.ToolSet }

func (t *terminalEnvironment) Instructions() string {
	return "get_environment_info reports the expected ACP client terminal shell, not the host's detected shell. Client and agent must use matching platforms."
}

func (t *terminalEnvironment) Tools(ctx context.Context) ([]tools.Tool, error) {
	definitions, err := t.ToolSet.Tools(ctx)
	if err != nil {
		return nil, err
	}
	for i := range definitions {
		definitions[i].Description = "Returns the expected ACP client terminal platform and shell; requires the same platform as the agent host."
		definitions[i].Handler = func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			command, _ := terminalInterpreter()
			return tools.ResultJSON(envtool.Info{OS: goruntime.GOOS, Shell: filepath.Base(command)}), nil
		}
	}
	return definitions, nil
}
