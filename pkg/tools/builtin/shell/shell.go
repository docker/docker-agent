package shell

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/docker/docker-agent/pkg/concurrent"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/shellpath"
	"github.com/docker/docker-agent/pkg/tools"
)

const (
	ToolNameShell              = "shell"
	ToolNameRunShellBackground = "run_background_job"
	ToolNameListBackgroundJobs = "list_background_jobs"
	ToolNameViewBackgroundJob  = "view_background_job"
	ToolNameStopBackgroundJob  = "stop_background_job"
)

// Tool provides shell command execution capabilities.
type Tool struct {
	handler *shellHandler
}

// Verify interface compliance
var (
	_ tools.ToolSet      = (*Tool)(nil)
	_ tools.Startable    = (*Tool)(nil)
	_ tools.Instructable = (*Tool)(nil)
)

type shellHandler struct {
	shell           string
	shellArgsPrefix []string
	env             []string
	timeout         time.Duration
	workingDir      string
	jobs            *concurrent.Map[string, *backgroundJob]
	jobCounter      atomic.Int64
}

// Job status constants
const (
	statusRunning int32 = iota
	statusCompleted
	statusStopped
	statusFailed
)

// backgroundJob tracks a background shell command
type backgroundJob struct {
	id           string
	cmd          string
	cwd          string
	process      *os.Process
	processGroup *processGroup
	outputMu     sync.RWMutex
	output       *bytes.Buffer
	startTime    time.Time
	status       atomic.Int32
	exitCode     int
	err          error
}

// limitedWriter wraps a buffer and stops writing after maxSize bytes.
// It uses an external mutex (mu) so that readers of the underlying buffer
// can share the same lock.
type limitedWriter struct {
	mu      *sync.RWMutex
	buf     *bytes.Buffer
	written int64
	maxSize int64
}

func (lw *limitedWriter) Write(p []byte) (n int, err error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()

	if remaining := lw.maxSize - lw.written; remaining > 0 {
		toWrite := min(int64(len(p)), remaining)
		lw.buf.Write(p[:toWrite]) // bytes.Buffer.Write never errors
		lw.written += toWrite
	}
	return len(p), nil // always report full write
}

type RunShellArgs struct {
	Cmd     string `json:"cmd" jsonschema:"Shell command"`
	Cwd     string `json:"cwd,omitempty" jsonschema:"Working directory (default \".\")"`
	Timeout int    `json:"timeout,omitempty" jsonschema:"Timeout in seconds (default 30)"`
}

// UnmarshalJSON accepts both the canonical "cmd" key and the common alias
// "command" for the shell command parameter.
//
// The advertised schema still declares "cmd" as the canonical name, but many
// models (particularly ones biased by Anthropic's built-in bash tool and other
// ecosystems that use "command") occasionally emit "command" instead. Accepting
// both prevents a wasted turn on an empty-command error while keeping the
// canonical contract unchanged. When "cmd" is present with a non-blank value
// it wins; a blank (empty or whitespace-only) "cmd" falls back to "command"
// so a valid alias is not silently shadowed.
func (a *RunShellArgs) UnmarshalJSON(data []byte) error {
	var raw struct {
		Cmd     string `json:"cmd"`
		Command string `json:"command"`
		Cwd     string `json:"cwd"`
		Timeout int    `json:"timeout"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	a.Cmd = preferNonBlank(raw.Cmd, raw.Command)
	a.Cwd = raw.Cwd
	a.Timeout = raw.Timeout
	return nil
}

type RunShellBackgroundArgs struct {
	Cmd string `json:"cmd" jsonschema:"Shell command to run in background"`
	Cwd string `json:"cwd,omitempty" jsonschema:"Working directory (default \".\")"`
}

// UnmarshalJSON accepts both "cmd" (canonical) and "command" (common alias),
// mirroring RunShellArgs.UnmarshalJSON. See its comment for rationale.
func (a *RunShellBackgroundArgs) UnmarshalJSON(data []byte) error {
	var raw struct {
		Cmd     string `json:"cmd"`
		Command string `json:"command"`
		Cwd     string `json:"cwd"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	a.Cmd = preferNonBlank(raw.Cmd, raw.Command)
	a.Cwd = raw.Cwd
	return nil
}

// preferNonBlank returns primary when it has a non-whitespace character;
// otherwise it returns fallback. The chosen value is returned unmodified so
// that whitespace inside a legitimate command (e.g. trailing newlines in a
// heredoc) is preserved.
func preferNonBlank(primary, fallback string) string {
	if strings.TrimSpace(primary) != "" {
		return primary
	}
	return fallback
}

type ViewBackgroundJobArgs struct {
	JobID string `json:"job_id" jsonschema:"Background job ID"`
}

type StopBackgroundJobArgs struct {
	JobID string `json:"job_id" jsonschema:"Background job ID"`
}

// statusStrings maps job status constants to their string representations
var statusStrings = map[int32]string{
	statusRunning:   "running",
	statusCompleted: "completed",
	statusStopped:   "stopped",
	statusFailed:    "failed",
}

func statusToString(status int32) string {
	if s, ok := statusStrings[status]; ok {
		return s
	}
	return "unknown"
}

func (h *shellHandler) RunShell(ctx context.Context, params RunShellArgs) (*tools.ToolCallResult, error) {
	if strings.TrimSpace(params.Cmd) == "" {
		return tools.ResultError(`Error: missing or empty "cmd" parameter. Pass the shell command as {"cmd": "..."}.`), nil
	}

	timeout := h.timeout
	if params.Timeout > 0 {
		timeout = time.Duration(params.Timeout) * time.Second
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cwd := h.resolveWorkDir(params.Cwd)

	slog.DebugContext(ctx, "Executing native shell command", "command", params.Cmd, "cwd", cwd)

	return h.runNativeCommand(timeoutCtx, ctx, params.Cmd, cwd, timeout), nil
}

// waitDelayAfterShellExit caps how long cmd.Wait() blocks on stdout/stderr
// copy goroutines after the direct shell child has exited.
//
// When cmd.Stdout/Stderr are not *os.File, Go's exec package creates OS pipes
// and spawns copy goroutines; cmd.Wait() only returns after *both* the child
// exits and those goroutines see EOF on the pipes. If the command backgrounds
// a grandchild (e.g. `docker run ... &`, `sleep 10 &`) that inherits the pipe
// fds, the pipes stay open and Wait() blocks until the configured timeout.
//
// cmd.WaitDelay tells Go to force-close the pipes and return this long after
// the direct child has exited, letting the grandchild keep running while the
// tool call returns promptly. A short delay is plenty because any output the
// shell itself produced is already flushed by the time it exits.
const waitDelayAfterShellExit = 500 * time.Millisecond

func (h *shellHandler) runNativeCommand(timeoutCtx, ctx context.Context, command, cwd string, timeout time.Duration) *tools.ToolCallResult {
	// Cancellation is handled manually below (timeoutCtx + Process.Kill +
	// process group + WaitDelay), so we use exec.Command rather than
	// exec.CommandContext to keep that flow in one place.
	cmd := exec.Command(h.shell, append(h.shellArgsPrefix, command)...) //nolint:noctx // see comment above
	cmd.Env = h.env
	cmd.Dir = cwd
	cmd.SysProcAttr = platformSpecificSysProcAttr()
	cmd.WaitDelay = waitDelayAfterShellExit

	var outBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &outBuf

	if err := cmd.Start(); err != nil {
		return tools.ResultError(fmt.Sprintf("Error starting command: %s", err))
	}

	pg, err := createProcessGroup(cmd.Process)
	if err != nil {
		// Successfully started the child but couldn't install it in its own
		// process group: clean it up before bailing out.
		reapSpawnedChild(cmd, pg)
		return tools.ResultError(fmt.Sprintf("Error creating process group: %s", err))
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	var cmdErr error
	select {
	case <-timeoutCtx.Done():
		_ = kill(cmd.Process, pg)
		// Wait for cmd.Wait() to complete so that the internal pipe-copy
		// goroutines finish writing to outBuf before we read it.
		// Use a grace period: if SIGTERM is ignored, escalate to SIGKILL.
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	case cmdErr = <-done:
	}

	output := formatCommandOutput(timeoutCtx, ctx, cmdErr, outBuf.String(), timeout)
	return tools.ResultSuccess(limitOutput(output))
}

func (h *shellHandler) RunShellBackground(_ context.Context, params RunShellBackgroundArgs) (*tools.ToolCallResult, error) {
	if strings.TrimSpace(params.Cmd) == "" {
		return tools.ResultError(`Error: missing or empty "cmd" parameter. Pass the shell command as {"cmd": "..."}.`), nil
	}

	counter := h.jobCounter.Add(1)
	jobID := fmt.Sprintf("job_%d_%d", time.Now().Unix(), counter)

	cmd := exec.Command(h.shell, append(h.shellArgsPrefix, params.Cmd)...) //nolint:noctx // RunShellBackground intentionally outlives the request context
	cmd.Env = h.env
	cmd.Dir = h.resolveWorkDir(params.Cwd)
	cmd.SysProcAttr = platformSpecificSysProcAttr()

	job := &backgroundJob{
		id:        jobID,
		cmd:       params.Cmd,
		cwd:       params.Cwd,
		output:    &bytes.Buffer{},
		startTime: time.Now(),
	}

	// The limitedWriter shares the job's outputMu so that readers
	// (ViewBackgroundJob, ListBackgroundJobs) and the pipe-copy
	// goroutines spawned by exec.Cmd use the same lock.
	lw := &limitedWriter{mu: &job.outputMu, buf: job.output, maxSize: 10 * 1024 * 1024}
	cmd.Stdout = lw
	cmd.Stderr = lw

	if err := cmd.Start(); err != nil {
		return tools.ResultError(fmt.Sprintf("Error starting background command: %s", err)), nil
	}

	pg, err := createProcessGroup(cmd.Process)
	if err != nil {
		// Successfully started the child but couldn't install it in its own
		// process group: clean it up before bailing out.
		reapSpawnedChild(cmd, pg)
		return tools.ResultError(fmt.Sprintf("Error creating process group: %s", err)), nil
	}

	job.process = cmd.Process
	job.processGroup = pg
	job.status.Store(statusRunning)
	h.jobs.Store(jobID, job)

	go h.monitorJob(job, cmd)

	return tools.ResultSuccess(fmt.Sprintf("Background job started with ID: %s\nCommand: %s\nWorking directory: %s",
		jobID, params.Cmd, params.Cwd)), nil
}

func (h *shellHandler) monitorJob(job *backgroundJob, cmd *exec.Cmd) {
	err := cmd.Wait()

	job.outputMu.Lock()
	defer job.outputMu.Unlock()

	if job.status.Load() == statusStopped {
		return
	}

	if err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			job.exitCode = exitErr.ExitCode()
		} else {
			job.exitCode = -1
		}
		job.status.Store(statusFailed)
		job.err = err
	} else {
		job.exitCode = 0
		job.status.Store(statusCompleted)
	}
}

func (h *shellHandler) ListBackgroundJobs(_ context.Context, _ map[string]any) (*tools.ToolCallResult, error) {
	var output strings.Builder
	output.WriteString("Background Jobs:\n\n")

	jobCount := 0
	h.jobs.Range(func(jobID string, job *backgroundJob) bool {
		jobCount++
		status := job.status.Load()
		elapsed := time.Since(job.startTime).Round(time.Second)

		fmt.Fprintf(&output, "ID: %s\n", jobID)
		fmt.Fprintf(&output, "  Command: %s\n", job.cmd)
		fmt.Fprintf(&output, "  Status: %s\n", statusToString(status))
		fmt.Fprintf(&output, "  Runtime: %s\n", elapsed)
		if status != statusRunning {
			job.outputMu.RLock()
			fmt.Fprintf(&output, "  Exit Code: %d\n", job.exitCode)
			job.outputMu.RUnlock()
		}
		output.WriteString("\n")
		return true
	})

	if jobCount == 0 {
		output.WriteString("No background jobs found.\n")
	}

	return tools.ResultSuccess(output.String()), nil
}

func (h *shellHandler) ViewBackgroundJob(_ context.Context, params ViewBackgroundJobArgs) (*tools.ToolCallResult, error) {
	job, exists := h.jobs.Load(params.JobID)
	if !exists {
		return tools.ResultError("Job not found: " + params.JobID), nil
	}

	status := job.status.Load()

	job.outputMu.RLock()
	output := job.output.String()
	exitCode := job.exitCode
	job.outputMu.RUnlock()

	var result strings.Builder
	fmt.Fprintf(&result, "Job ID: %s\n", job.id)
	fmt.Fprintf(&result, "Command: %s\n", job.cmd)
	fmt.Fprintf(&result, "Status: %s\n", statusToString(status))
	fmt.Fprintf(&result, "Runtime: %s\n", time.Since(job.startTime).Round(time.Second))
	if status != statusRunning {
		fmt.Fprintf(&result, "Exit Code: %d\n", exitCode)
	}
	result.WriteString("\n--- Output ---\n")
	if output == "" {
		result.WriteString("<no output>\n")
	} else {
		result.WriteString(output)
		if len(output) >= 10*1024*1024 {
			result.WriteString("\n\n[Output truncated at 10MB limit]")
		}
	}

	return tools.ResultSuccess(result.String()), nil
}

func (h *shellHandler) StopBackgroundJob(_ context.Context, params StopBackgroundJobArgs) (*tools.ToolCallResult, error) {
	job, exists := h.jobs.Load(params.JobID)
	if !exists {
		return tools.ResultError("Job not found: " + params.JobID), nil
	}

	if !job.status.CompareAndSwap(statusRunning, statusStopped) {
		currentStatus := job.status.Load()
		return tools.ResultError(fmt.Sprintf("Job %s is not running (current status: %s)", params.JobID, statusToString(currentStatus))), nil
	}

	if err := kill(job.process, job.processGroup); err != nil {
		return tools.ResultError(fmt.Sprintf("Job %s marked as stopped, but error killing process: %s", params.JobID, err)), nil
	}

	return tools.ResultSuccess(fmt.Sprintf("Job %s stopped successfully", params.JobID)), nil
}

// reapSpawnedChild terminates a child that we've started but decided not
// to run (e.g. follow-up setup failed) and waits for it so we don't leak a
// zombie or its stdout/stderr pipes. SIGTERM is sent first; if the child
// hasn't exited after a short grace period we escalate to SIGKILL.
func reapSpawnedChild(cmd *exec.Cmd, pg *processGroup) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = kill(cmd.Process, pg)

	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
}

// NewShellTool creates a new shell tool.
func NewShellTool(env []string, runConfig *config.RuntimeConfig) *Tool {
	shell, argsPrefix := detectShell()

	handler := &shellHandler{
		shell:           shell,
		shellArgsPrefix: argsPrefix,
		env:             env,
		timeout:         30 * time.Second,
		jobs:            concurrent.NewMap[string, *backgroundJob](),
		workingDir:      runConfig.WorkingDir,
	}

	return &Tool{handler: handler}
}

// detectShell returns the appropriate shell and arguments based on the platform.
// It delegates to shellpath.DetectShell which uses absolute paths to prevent
// PATH hijacking (CWE-426).
func detectShell() (shell string, argsPrefix []string) {
	return shellpath.DetectShell()
}

// resolveWorkDir returns the effective working directory.
func (h *shellHandler) resolveWorkDir(cwd string) string {
	if cwd == "" || cwd == "." {
		return h.workingDir
	}
	if !filepath.IsAbs(cwd) {
		return filepath.Clean(filepath.Join(h.workingDir, cwd))
	}
	return cwd
}

// formatCommandOutput formats command output handling timeout, cancellation, and errors.
func formatCommandOutput(timeoutCtx, ctx context.Context, err error, rawOutput string, timeout time.Duration) string {
	var output string
	if timeoutCtx.Err() != nil {
		if ctx.Err() != nil {
			output = "Command cancelled"
		} else {
			output = fmt.Sprintf("Command timed out after %v\nOutput: %s", timeout, rawOutput)
		}
	} else {
		output = rawOutput
		if err != nil {
			output = fmt.Sprintf("Error executing command: %s\nOutput: %s", err, output)
		}
	}
	return cmp.Or(strings.TrimSpace(output), "<no output>")
}

func (t *Tool) Instructions() string {
	return `## Shell Tools

- Each call runs in a fresh shell session — no state persists between calls
- Default timeout: 30s. Set "timeout" for longer operations (builds, tests)
- Use "cwd" parameter instead of cd within commands
- Combine operations with pipes, redirections, and heredocs
- Non-zero exit codes return error info with output; timed-out commands are terminated

### Background Jobs

Use run_background_job for long-running processes (servers, watchers). Output capped at 10MB per job. All jobs auto-terminate when the agent stops.`
}

func (t *Tool) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{
		{
			Name:                    ToolNameShell,
			Category:                "shell",
			Description:             `Executes the given shell command in the user's default shell.`,
			Parameters:              tools.MustSchemaFor[RunShellArgs](),
			OutputSchema:            tools.MustSchemaFor[string](),
			Handler:                 tools.NewHandler(t.handler.RunShell),
			Annotations:             tools.ToolAnnotations{Title: "Shell"},
			AddDescriptionParameter: true,
		},
		{
			Name:                    ToolNameRunShellBackground,
			Category:                "shell",
			Description:             `Starts a shell command in the background and returns immediately with a job ID. Use this for long-running processes like servers, watches, or any command that should run while other tasks are performed.`,
			Parameters:              tools.MustSchemaFor[RunShellBackgroundArgs](),
			OutputSchema:            tools.MustSchemaFor[string](),
			Handler:                 tools.NewHandler(t.handler.RunShellBackground),
			Annotations:             tools.ToolAnnotations{Title: "Background Job"},
			AddDescriptionParameter: true,
		},
		{
			Name:                    ToolNameListBackgroundJobs,
			Category:                "shell",
			Description:             `Lists all background jobs with their status, runtime, and other information.`,
			OutputSchema:            tools.MustSchemaFor[string](),
			Handler:                 tools.NewHandler(t.handler.ListBackgroundJobs),
			Annotations:             tools.ToolAnnotations{Title: "List Background Jobs", ReadOnlyHint: true},
			AddDescriptionParameter: true,
		},
		{
			Name:                    ToolNameViewBackgroundJob,
			Category:                "shell",
			Description:             `Views the output and status of a specific background job by job ID.`,
			Parameters:              tools.MustSchemaFor[ViewBackgroundJobArgs](),
			OutputSchema:            tools.MustSchemaFor[string](),
			Handler:                 tools.NewHandler(t.handler.ViewBackgroundJob),
			Annotations:             tools.ToolAnnotations{Title: "View Background Job Output", ReadOnlyHint: true},
			AddDescriptionParameter: true,
		},
		{
			Name:                    ToolNameStopBackgroundJob,
			Category:                "shell",
			Description:             `Stops a running background job by job ID. The process and all its child processes will be terminated.`,
			Parameters:              tools.MustSchemaFor[StopBackgroundJobArgs](),
			OutputSchema:            tools.MustSchemaFor[string](),
			Handler:                 tools.NewHandler(t.handler.StopBackgroundJob),
			Annotations:             tools.ToolAnnotations{Title: "Stop Background Job"},
			AddDescriptionParameter: true,
		},
	}, nil
}

func (t *Tool) Start(context.Context) error {
	return nil
}

func (t *Tool) Stop(context.Context) error {
	// Terminate all running background jobs
	t.handler.jobs.Range(func(_ string, job *backgroundJob) bool {
		if job.status.CompareAndSwap(statusRunning, statusStopped) {
			_ = kill(job.process, job.processGroup)
		}
		return true
	})

	return nil
}
