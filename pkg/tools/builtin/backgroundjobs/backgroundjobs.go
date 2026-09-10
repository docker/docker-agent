// Package backgroundjobs exposes the built-in background_jobs toolset.
package backgroundjobs

import (
	"bytes"
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
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/safety"
	"github.com/docker/docker-agent/pkg/shellpath"
	"github.com/docker/docker-agent/pkg/tools"
)

const (
	// ToolNameRunBackgroundJob starts a shell command asynchronously.
	ToolNameRunBackgroundJob = "run_background_job"
	// ToolNameListBackgroundJobs lists known background jobs.
	ToolNameListBackgroundJobs = "list_background_jobs"
	// ToolNameViewBackgroundJob renders one background job's status and output.
	ToolNameViewBackgroundJob = "view_background_job"
	// ToolNameStopBackgroundJob stops a running background job.
	ToolNameStopBackgroundJob = "stop_background_job"
	// ToolNameWaitBackgroundJob waits for a background job to finish.
	ToolNameWaitBackgroundJob = "wait_background_job"

	maxBackgroundJobOutputBytes = 10 * 1024 * 1024

	// waitDelayAfterJobExit specifies how long cmd.Wait waits for I/O pipes to close
	// after the main process exits. Like the shell tool, this prevents hangs when a
	// background job daemonizes (e.g. `myserver &`) and the grandchild inherits
	// stdout/stderr. If the delay expires, Wait() returns exec.ErrWaitDelay and the
	// pipes are forcefully closed. This truncates captured output from the still-running
	// grandchild and typically kills it with SIGPIPE on its next write. This trade-off
	// is necessary: run_background_job is designed to exit its direct child quickly
	// so the agent can continue its turn. If a user needs a permanently-running job
	// that survives indefinitely while logging, they should redirect its output
	// (e.g. `myserver > log.txt 2>&1 &`).
	waitDelayAfterJobExit = 1 * time.Second
	gracefulStopTimeout   = 500 * time.Millisecond
	forcedStopTimeout     = 2 * time.Second
)

// ToolSet manages long-running shell commands.
type ToolSet struct {
	handler *backgroundJobsHandler
}

// Verify interface compliance
var (
	_ tools.ToolSet      = (*ToolSet)(nil)
	_ tools.Startable    = (*ToolSet)(nil)
	_ tools.Instructable = (*ToolSet)(nil)
)

type backgroundJobsHandler struct {
	shell           string
	shellArgsPrefix []string
	env             []string
	workingDir      string
	jobs            *concurrent.Map[string, *backgroundJob]
	jobCounter      atomic.Int64
	recall          bool
}

const (
	statusRunning int32 = iota
	statusCompleted
	statusStopped
	statusFailed
)

type backgroundJob struct {
	id            string
	cmd           string
	cwd           string
	process       *os.Process
	processGroup  *processGroup
	outputMu      sync.RWMutex
	output        *bytes.Buffer
	startTime     time.Time
	status        atomic.Int32
	exitCode      int
	err           error
	done          chan struct{}
	stopRequested atomic.Bool
	stopMu        sync.Mutex
	rt            tools.Runtime
}

// limitedWriter wraps a buffer and stops writing after maxSize bytes. It uses
// an external mutex so readers of the underlying buffer can share the same lock.
type limitedWriter struct {
	mu      *sync.RWMutex
	buf     *bytes.Buffer
	written int64
	maxSize int64
}

func (lw *limitedWriter) Write(p []byte) (int, error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()

	if remaining := lw.maxSize - lw.written; remaining > 0 {
		toWrite := min(int64(len(p)), remaining)
		lw.buf.Write(p[:toWrite]) // bytes.Buffer.Write never errors
		lw.written += toWrite
	}
	return len(p), nil
}

// RunBackgroundJobArgs contains the parameters for run_background_job.
type RunBackgroundJobArgs struct {
	Cmd string `json:"cmd" jsonschema:"Shell command to run in background"`
	Cwd string `json:"cwd,omitempty" jsonschema:"Working directory (default \".\")"`
}

// RunBackgroundJobRecallArgs contains the parameters for run_background_job when recall is enabled.
type RunBackgroundJobRecallArgs struct {
	Cmd    string `json:"cmd" jsonschema:"Shell command to run in background"`
	Cwd    string `json:"cwd,omitempty" jsonschema:"Working directory (default \".\")"`
	Recall bool   `json:"recall,omitempty" jsonschema:"Ask to be recalled with a steering message when the background job finishes"`
}

// UnmarshalJSON accepts both "cmd" (canonical) and "command" (common alias),
// resolved by [safety.CommandArg] over an exact-key map so the executed
// command is the one the runtime classified (see shell.RunShellArgs).
func (a *RunBackgroundJobArgs) UnmarshalJSON(data []byte) error {
	var raw struct {
		Cwd string `json:"cwd"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	cmd, err := commandField(data)
	if err != nil {
		return err
	}
	a.Cmd = cmd
	a.Cwd = raw.Cwd
	return nil
}

// UnmarshalJSON accepts both "cmd" (canonical) and "command" (common alias),
// resolved like [RunBackgroundJobArgs].
func (a *RunBackgroundJobRecallArgs) UnmarshalJSON(data []byte) error {
	var raw struct {
		Cwd    string `json:"cwd"`
		Recall bool   `json:"recall"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	cmd, err := commandField(data)
	if err != nil {
		return err
	}
	a.Cmd = cmd
	a.Cwd = raw.Cwd
	a.Recall = raw.Recall
	return nil
}

func commandField(data []byte) (string, error) {
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		return "", err
	}
	cmd, _ := safety.CommandArg(fields)
	return cmd, nil
}

// ViewBackgroundJobArgs contains the parameters for view_background_job.
type ViewBackgroundJobArgs struct {
	JobID string `json:"job_id" jsonschema:"Background job ID"`
}

// StopBackgroundJobArgs contains the parameters for stop_background_job.
type StopBackgroundJobArgs struct {
	JobID string `json:"job_id" jsonschema:"Background job ID"`
}

// WaitBackgroundJobArgs contains the parameters for wait_background_job.
type WaitBackgroundJobArgs struct {
	JobID   string `json:"job_id" jsonschema:"Background job ID"`
	Timeout int    `json:"timeout,omitempty" jsonschema:"Maximum seconds to wait (default 60). Returns current output with a timeout notice if the job is still running when the limit is reached."`
}

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

type runBackgroundJobParams struct {
	Cmd    string
	Cwd    string
	Recall bool
}

func (h *backgroundJobsHandler) RunBackgroundJob(ctx context.Context, params RunBackgroundJobArgs, rt tools.Runtime) (*tools.ToolCallResult, error) {
	return h.runBackgroundJob(ctx, rt, runBackgroundJobParams{Cmd: params.Cmd, Cwd: params.Cwd})
}

func (h *backgroundJobsHandler) RunBackgroundJobWithRecall(ctx context.Context, params RunBackgroundJobRecallArgs, rt tools.Runtime) (*tools.ToolCallResult, error) {
	return h.runBackgroundJob(ctx, rt, runBackgroundJobParams(params))
}

func (h *backgroundJobsHandler) runBackgroundJob(ctx context.Context, rt tools.Runtime, params runBackgroundJobParams) (*tools.ToolCallResult, error) {
	if strings.TrimSpace(params.Cmd) == "" {
		return tools.ResultError(`Error: missing or empty "cmd" parameter. Pass the shell command as {"cmd": "..."}.`), nil
	}

	if params.Recall {
		if !h.recall {
			return tools.ResultError(`Error: "recall" is not enabled for this background_jobs toolset. Set recall: true on the background_jobs toolset before requesting recall.`), nil
		}
		if !rt.Supports(tools.CapabilityRecall) {
			return tools.ResultError(`Error: recall requested but the host does not support recall.`), nil
		}
	}

	counter := h.jobCounter.Add(1)
	jobID := fmt.Sprintf("job_%d_%d", time.Now().Unix(), counter)

	cmd := exec.Command(h.shell, append(h.shellArgsPrefix, params.Cmd)...) //nolint:noctx // background jobs intentionally outlive the request context
	cmd.Env = h.env
	cmd.Dir = h.resolveWorkDir(params.Cwd)
	cmd.SysProcAttr = platformSpecificSysProcAttr()
	cmd.WaitDelay = waitDelayAfterJobExit

	job := &backgroundJob{
		id:        jobID,
		cmd:       params.Cmd,
		cwd:       params.Cwd,
		output:    &bytes.Buffer{},
		startTime: time.Now(),
		done:      make(chan struct{}),
	}
	if params.Recall {
		job.rt = rt
	}

	lw := &limitedWriter{mu: &job.outputMu, buf: job.output, maxSize: maxBackgroundJobOutputBytes}
	cmd.Stdout = lw
	cmd.Stderr = lw

	if err := cmd.Start(); err != nil {
		return tools.ResultError(fmt.Sprintf("Error starting background command: %s", err)), nil
	}

	pg, err := createProcessGroup(cmd.Process)
	if err != nil {
		reapSpawnedChild(cmd, pg)
		return tools.ResultError(fmt.Sprintf("Error creating process group: %s", err)), nil
	}

	job.process = cmd.Process
	job.processGroup = pg
	job.status.Store(statusRunning)
	h.jobs.Store(jobID, job)

	go h.monitorJob(context.WithoutCancel(ctx), job, cmd)

	var recallStatus string
	if params.Recall {
		recallStatus = "\nRecall: requested; a steering message will be sent when the job finishes."
	}

	return tools.ResultSuccess(fmt.Sprintf("Background job started with ID: %s\nCommand: %s\nWorking directory: %s%s",
		jobID, params.Cmd, params.Cwd, recallStatus)), nil
}

func (h *backgroundJobsHandler) monitorJob(ctx context.Context, job *backgroundJob, cmd *exec.Cmd) {
	defer close(job.done)

	err := cmd.Wait()

	job.outputMu.Lock()

	var newStatus int32
	if err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			job.exitCode = exitErr.ExitCode()
		} else {
			job.exitCode = -1
		}
		newStatus = statusFailed
		job.err = err
	} else {
		// Either err == nil (clean exit) or err == exec.ErrWaitDelay (grandchild inherited pipes).
		job.exitCode = cmd.ProcessState.ExitCode()
		if job.exitCode == 0 {
			newStatus = statusCompleted
		} else {
			newStatus = statusFailed
		}
	}

	if job.stopRequested.Load() {
		newStatus = statusStopped
	}
	job.status.Store(newStatus)

	status := job.status.Load()
	exitCode := job.exitCode
	output := job.output.String()
	rt := job.rt
	job.outputMu.Unlock()

	if rt != nil {
		message := formatBackgroundJobRecall(job, status, exitCode, output)
		if err := rt.Recall(ctx, message); err != nil {
			slog.WarnContext(ctx, "Failed to enqueue background job recall", "job_id", job.id, "error", err)
		}
	}
}

func (h *backgroundJobsHandler) ListBackgroundJobs(_ context.Context, _ map[string]any) (*tools.ToolCallResult, error) {
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

func renderBackgroundJob(job *backgroundJob) string {
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
	switch status {
	case statusCompleted, statusFailed:
		fmt.Fprintf(&result, "Exit Code: %d\n", exitCode)
	}
	result.WriteString("\n--- Output ---\n")
	if output == "" {
		result.WriteString("<no output>\n")
	} else {
		result.WriteString(output)
		if len(output) >= maxBackgroundJobOutputBytes {
			result.WriteString("\n\n[Output truncated at 10MB limit]")
		}
	}
	return result.String()
}

func (h *backgroundJobsHandler) ViewBackgroundJob(_ context.Context, params ViewBackgroundJobArgs) (*tools.ToolCallResult, error) {
	job, exists := h.jobs.Load(params.JobID)
	if !exists {
		return tools.ResultError("Job not found: " + params.JobID), nil
	}
	return tools.ResultSuccess(renderBackgroundJob(job)), nil
}

const defaultWaitTimeout = 60 * time.Second

func (h *backgroundJobsHandler) WaitBackgroundJob(ctx context.Context, params WaitBackgroundJobArgs) (*tools.ToolCallResult, error) {
	job, exists := h.jobs.Load(params.JobID)
	if !exists {
		return tools.ResultError("Job not found: " + params.JobID), nil
	}

	timeout := defaultWaitTimeout
	if params.Timeout > 0 {
		timeout = time.Duration(params.Timeout) * time.Second
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-job.done:
		status := statusStrings[job.status.Load()]
		header := fmt.Sprintf("Job %s %s.\n\n", job.id, status)
		return tools.ResultSuccess(header + renderBackgroundJob(job)), nil
	case <-timer.C:
		header := fmt.Sprintf("Timed out after %s waiting for job %s; it is still running.\n\n",
			timeout.Round(time.Second), job.id)
		return tools.ResultSuccess(header + renderBackgroundJob(job)), nil
	case <-ctx.Done():
		return tools.ResultError(fmt.Sprintf("Wait cancelled for job %s: %s", job.id, ctx.Err())), nil
	}
}

func (h *backgroundJobsHandler) StopBackgroundJob(_ context.Context, params StopBackgroundJobArgs) (*tools.ToolCallResult, error) {
	job, exists := h.jobs.Load(params.JobID)
	if !exists {
		return tools.ResultError("Job not found: " + params.JobID), nil
	}

	if err := stopBackgroundJob(job); err != nil {
		return tools.ResultError("Error: " + err.Error()), nil
	}
	return tools.ResultSuccess(fmt.Sprintf("Job %s stopped successfully", params.JobID)), nil
}

func stopBackgroundJob(job *backgroundJob) error {
	job.stopMu.Lock()
	defer job.stopMu.Unlock()
	if job.status.Load() != statusRunning {
		return fmt.Errorf("job %s is not running (current status: %s)", job.id, statusToString(job.status.Load()))
	}

	if err := terminateProcess(job.process, job.processGroup, false); err != nil {
		if job.status.Load() == statusRunning {
			return fmt.Errorf("error stopping job %s: %w", job.id, err)
		}
		return nil
	}
	job.stopRequested.Store(true)
	if !waitForJob(job.done, gracefulStopTimeout) {
		if err := terminateProcess(job.process, job.processGroup, true); err != nil && job.status.Load() == statusRunning {
			return fmt.Errorf("error force-stopping job %s: %w", job.id, err)
		}
		if !waitForJob(job.done, forcedStopTimeout) {
			return fmt.Errorf("timed out waiting for job %s to stop", job.id)
		}
	}

	return nil
}

func waitForJob(done <-chan struct{}, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

func formatBackgroundJobRecall(job *backgroundJob, status int32, exitCode int, output string) string {
	var result strings.Builder
	fmt.Fprintf(&result, "Background job %s finished with status %s (exit code %d).\n", job.id, statusToString(status), exitCode)
	fmt.Fprintf(&result, "Command: %s\n\n", job.cmd)
	result.WriteString("--- Output ---\n")
	if output == "" {
		result.WriteString("<no output>\n")
	} else {
		result.WriteString(output)
		if len(output) >= maxBackgroundJobOutputBytes {
			result.WriteString("\n\n[Output truncated at 10MB limit]")
		}
	}
	return result.String()
}

func reapSpawnedChild(cmd *exec.Cmd, pg *processGroup) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = terminateProcess(cmd.Process, pg, false)

	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = terminateProcess(cmd.Process, pg, true)
		<-done
	}
}

// CreateToolSet builds the background_jobs toolset from config.
func CreateToolSet(ctx context.Context, toolset latest.Toolset, runConfig *config.RuntimeConfig) (tools.ToolSet, error) {
	env, err := toolsetEnv(ctx, toolset, runConfig)
	if err != nil {
		return nil, err
	}

	ts := New(env, runConfig)
	if toolset.Recall != nil && *toolset.Recall {
		ts.handler.recall = true
	}
	return ts, nil
}

func toolsetEnv(ctx context.Context, toolset latest.Toolset, runConfig *config.RuntimeConfig) ([]string, error) {
	env, err := environment.ExpandAll(ctx, environment.ToValues(toolset.Env), runConfig.EnvProvider())
	if err != nil {
		return nil, fmt.Errorf("failed to expand the toolset's environment variables: %w", err)
	}
	return append(os.Environ(), env...), nil
}

// New creates a background_jobs toolset with the supplied environment.
func New(env []string, runConfig *config.RuntimeConfig) *ToolSet {
	shell, argsPrefix := shellpath.DetectShell()

	handler := &backgroundJobsHandler{
		shell:           shell,
		shellArgsPrefix: argsPrefix,
		env:             env,
		jobs:            concurrent.NewMap[string, *backgroundJob](),
		workingDir:      runConfig.WorkingDir,
	}

	return &ToolSet{handler: handler}
}

func (h *backgroundJobsHandler) resolveWorkDir(cwd string) string {
	if cwd == "" || cwd == "." {
		return h.workingDir
	}
	if !filepath.IsAbs(cwd) {
		return filepath.Clean(filepath.Join(h.workingDir, cwd))
	}
	return cwd
}

func (t *ToolSet) Instructions() string {
	instructions := `## Background Job Tools

Use run_background_job for long-running processes (servers, watchers). Output capped at 10MB per job. All jobs auto-terminate when the agent stops.

Use wait_background_job to block until a job finishes and retrieve its exit code and full output. Pass an optional timeout (seconds) to cap how long to wait; the job keeps running if the timeout fires.`
	if t.handler.recall {
		instructions += `

When starting a background job, set "recall": true if you want the agent loop to be steered automatically with a short completion message and the job output when it finishes.`
	}
	return instructions
}

func (t *ToolSet) runBackgroundJobDescription() string {
	description := `Starts a shell command in the background and returns immediately with a job ID. Use this for long-running processes like servers, watches, or any command that should run while other tasks are performed.`
	if t.handler.recall {
		description += ` Set recall=true to receive a steering message with the job output when it finishes.`
	}
	return description
}

func runBackgroundJobParameters(recall bool) any {
	if recall {
		return tools.MustSchemaFor[RunBackgroundJobRecallArgs]()
	}
	return tools.MustSchemaFor[RunBackgroundJobArgs]()
}

func (t *ToolSet) runBackgroundJobHandler() tools.ToolHandler {
	if t.handler.recall {
		return tools.NewRuntimeHandler(t.handler.RunBackgroundJobWithRecall)
	}
	return tools.NewRuntimeHandler(t.handler.RunBackgroundJob)
}

func (t *ToolSet) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{
		{
			Name:                    ToolNameRunBackgroundJob,
			Category:                "background_jobs",
			Description:             t.runBackgroundJobDescription(),
			Parameters:              runBackgroundJobParameters(t.handler.recall),
			OutputSchema:            tools.MustSchemaFor[string](),
			Handler:                 t.runBackgroundJobHandler(),
			Annotations:             tools.ToolAnnotations{Title: "Background Job"},
			AddDescriptionParameter: true,
		},
		{
			Name:                    ToolNameListBackgroundJobs,
			Category:                "background_jobs",
			Description:             `Lists all background jobs with their status, runtime, and other information.`,
			OutputSchema:            tools.MustSchemaFor[string](),
			Handler:                 tools.NewHandler(t.handler.ListBackgroundJobs),
			Annotations:             tools.ToolAnnotations{Title: "List Background Jobs", ReadOnlyHint: true},
			AddDescriptionParameter: true,
		},
		{
			Name:                    ToolNameViewBackgroundJob,
			Category:                "background_jobs",
			Description:             `Views the output and status of a specific background job by job ID.`,
			Parameters:              tools.MustSchemaFor[ViewBackgroundJobArgs](),
			OutputSchema:            tools.MustSchemaFor[string](),
			Handler:                 tools.NewHandler(t.handler.ViewBackgroundJob),
			Annotations:             tools.ToolAnnotations{Title: "View Background Job Output", ReadOnlyHint: true},
			AddDescriptionParameter: true,
		},
		{
			Name:                    ToolNameStopBackgroundJob,
			Category:                "background_jobs",
			Description:             `Stops a running background job by job ID. The process and all its child processes will be terminated.`,
			Parameters:              tools.MustSchemaFor[StopBackgroundJobArgs](),
			OutputSchema:            tools.MustSchemaFor[string](),
			Handler:                 tools.NewHandler(t.handler.StopBackgroundJob),
			Annotations:             tools.ToolAnnotations{Title: "Stop Background Job"},
			AddDescriptionParameter: true,
		},
		{
			Name:                    ToolNameWaitBackgroundJob,
			Category:                "background_jobs",
			Description:             `Blocks until a background job completes (or the optional timeout expires) and returns its exit code and captured output. Safe to call on an already-finished job — returns the cached result immediately. Use this instead of polling view_background_job in a loop.`,
			Parameters:              tools.MustSchemaFor[WaitBackgroundJobArgs](),
			OutputSchema:            tools.MustSchemaFor[string](),
			Handler:                 tools.NewHandler(t.handler.WaitBackgroundJob),
			Annotations:             tools.ToolAnnotations{Title: "Wait for Background Job", ReadOnlyHint: true},
			AddDescriptionParameter: true,
		},
	}, nil
}

func (t *ToolSet) Start(context.Context) error {
	return nil
}

func (t *ToolSet) Stop(context.Context) error {
	var wg sync.WaitGroup
	errs := make(chan error)
	t.handler.jobs.Range(func(_ string, job *backgroundJob) bool {
		if job.status.Load() == statusRunning {
			wg.Go(func() {
				if err := stopBackgroundJob(job); err != nil {
					errs <- err
				}
			})
		}
		return true
	})
	go func() {
		wg.Wait()
		close(errs)
	}()

	var stopErr error
	for err := range errs {
		stopErr = errors.Join(stopErr, err)
	}
	return stopErr
}
