package backgroundjobs

import (
	"context"
	"encoding/json"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/safety"
	"github.com/docker/docker-agent/pkg/tools"
)

func newTestTool(t *testing.T) *ToolSet {
	t.Helper()
	return New(nil, &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}})
}

// pkg/safety duplicates the tool name to classify background commands
// without importing this package; a rename must update both.
func TestToolNameMatchesSafetyPackage(t *testing.T) {
	t.Parallel()
	assert.Equal(t, ToolNameRunBackgroundJob, safety.BackgroundJobToolName)
}

func TestRunBackgroundJobArgs_UnmarshalJSON_AcceptsCmdAndCommand(t *testing.T) {
	t.Parallel()

	var viaCmd RunBackgroundJobArgs
	require.NoError(t, json.Unmarshal([]byte(`{"cmd":"sleep 1","cwd":"/tmp"}`), &viaCmd))
	assert.Equal(t, "sleep 1", viaCmd.Cmd)
	assert.Equal(t, "/tmp", viaCmd.Cwd)

	var viaCommand RunBackgroundJobArgs
	require.NoError(t, json.Unmarshal([]byte(`{"command":"sleep 1"}`), &viaCommand))
	assert.Equal(t, "sleep 1", viaCommand.Cmd)

	var blankCmd RunBackgroundJobArgs
	require.NoError(t, json.Unmarshal([]byte(`{"cmd":"   ","command":"sleep 1"}`), &blankCmd))
	assert.Equal(t, "sleep 1", blankCmd.Cmd)

	// Mixed-case keys must not override the exact key the runtime
	// classified (encoding/json struct decoding would let them).
	for _, input := range []string{
		`{"cmd":"git status","CMD":"rm -rf /tmp/x"}`,
		`{"command":"git status","Command":"rm -rf /tmp/x"}`,
	} {
		var plain RunBackgroundJobArgs
		require.NoError(t, json.Unmarshal([]byte(input), &plain))
		var recall RunBackgroundJobRecallArgs
		require.NoError(t, json.Unmarshal([]byte(input), &recall))

		var fields map[string]any
		require.NoError(t, json.Unmarshal([]byte(input), &fields))
		classified, _ := safety.CommandArg(fields)
		assert.Equal(t, "git status", classified)
		assert.Equal(t, classified, plain.Cmd, input)
		assert.Equal(t, classified, recall.Cmd, input)
	}
}

func TestRunBackgroundJobRecallArgs_UnmarshalJSON_AcceptsRecall(t *testing.T) {
	t.Parallel()

	var withRecall RunBackgroundJobRecallArgs
	require.NoError(t, json.Unmarshal([]byte(`{"command":"sleep 1","cwd":"/tmp","recall":true}`), &withRecall))
	assert.Equal(t, "sleep 1", withRecall.Cmd)
	assert.Equal(t, "/tmp", withRecall.Cwd)
	assert.True(t, withRecall.Recall)
}

func TestBackgroundJobsTool_OutputSchema(t *testing.T) {
	t.Parallel()
	tool := newTestTool(t)

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, allTools)

	for _, tool := range allTools {
		assert.NotNil(t, tool.OutputSchema)
	}
}

func TestBackgroundJobsTool_ParametersAreObjects(t *testing.T) {
	t.Parallel()
	tool := newTestTool(t)

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, allTools)

	for _, tool := range allTools {
		m, err := tools.SchemaToMap(tool.Parameters)
		require.NoError(t, err)
		assert.Equal(t, "object", m["type"])
	}
}

func TestBackgroundJobsTool_WaitBackgroundJob_Completed(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell semantics; skipped on Windows")
	}
	tool := newTestTool(t)
	require.NoError(t, tool.Start(t.Context()))
	t.Cleanup(func() { _ = tool.Stop(t.Context()) })

	_, err := tool.handler.RunBackgroundJob(t.Context(), RunBackgroundJobArgs{Cmd: "echo hello"}, tools.NopRuntime{})
	require.NoError(t, err)

	var jobID string
	tool.handler.jobs.Range(func(id string, _ *backgroundJob) bool {
		jobID = id
		return false
	})
	require.NotEmpty(t, jobID)

	result, err := tool.handler.WaitBackgroundJob(t.Context(), WaitBackgroundJobArgs{JobID: jobID})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "completed")
	assert.Contains(t, result.Output, "Exit Code: 0")
	assert.Contains(t, result.Output, "hello")
}

func TestBackgroundJobsTool_WaitBackgroundJob_FailedExit(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell semantics; skipped on Windows")
	}
	tool := newTestTool(t)
	require.NoError(t, tool.Start(t.Context()))
	t.Cleanup(func() { _ = tool.Stop(t.Context()) })

	_, err := tool.handler.RunBackgroundJob(t.Context(), RunBackgroundJobArgs{Cmd: "exit 3"}, tools.NopRuntime{})
	require.NoError(t, err)

	var jobID string
	tool.handler.jobs.Range(func(id string, _ *backgroundJob) bool {
		jobID = id
		return false
	})
	require.NotEmpty(t, jobID)

	result, err := tool.handler.WaitBackgroundJob(t.Context(), WaitBackgroundJobArgs{JobID: jobID})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "failed")
	assert.Contains(t, result.Output, "Exit Code: 3")
}

func TestBackgroundJobsTool_WaitBackgroundJob_AlreadyCompleted(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell semantics; skipped on Windows")
	}
	tool := newTestTool(t)
	require.NoError(t, tool.Start(t.Context()))
	t.Cleanup(func() { _ = tool.Stop(t.Context()) })

	_, err := tool.handler.RunBackgroundJob(t.Context(), RunBackgroundJobArgs{Cmd: "echo done"}, tools.NopRuntime{})
	require.NoError(t, err)

	var jobID string
	tool.handler.jobs.Range(func(id string, _ *backgroundJob) bool {
		jobID = id
		return false
	})
	require.NotEmpty(t, jobID)

	result1, err := tool.handler.WaitBackgroundJob(t.Context(), WaitBackgroundJobArgs{JobID: jobID})
	require.NoError(t, err)
	assert.Contains(t, result1.Output, "completed")

	result2, err := tool.handler.WaitBackgroundJob(t.Context(), WaitBackgroundJobArgs{JobID: jobID})
	require.NoError(t, err)
	assert.Contains(t, result2.Output, "completed")
	assert.Contains(t, result2.Output, "Exit Code: 0")
}

func TestBackgroundJobsTool_WaitBackgroundJob_UnknownID(t *testing.T) {
	t.Parallel()
	tool := newTestTool(t)

	result, err := tool.handler.WaitBackgroundJob(t.Context(), WaitBackgroundJobArgs{JobID: "job_does_not_exist"})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "Job not found")
}

func TestBackgroundJobsTool_WaitBackgroundJob_Timeout(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("sleep not available on Windows cmd")
	}
	tool := newTestTool(t)
	require.NoError(t, tool.Start(t.Context()))
	t.Cleanup(func() { _ = tool.Stop(t.Context()) })

	_, err := tool.handler.RunBackgroundJob(t.Context(), RunBackgroundJobArgs{Cmd: "sleep 30"}, tools.NopRuntime{})
	require.NoError(t, err)

	var jobID string
	tool.handler.jobs.Range(func(id string, _ *backgroundJob) bool {
		jobID = id
		return false
	})
	require.NotEmpty(t, jobID)

	start := time.Now()
	result, err := tool.handler.WaitBackgroundJob(t.Context(), WaitBackgroundJobArgs{JobID: jobID, Timeout: 1})
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.Contains(t, result.Output, "Timed out")
	assert.Contains(t, result.Output, "still running")
	assert.Less(t, elapsed, 5*time.Second)
}

func TestBackgroundJobsTool_WaitBackgroundJob_ContextCancelled(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("sleep not available on Windows cmd")
	}
	tool := newTestTool(t)
	require.NoError(t, tool.Start(t.Context()))
	t.Cleanup(func() { _ = tool.Stop(t.Context()) })

	_, err := tool.handler.RunBackgroundJob(t.Context(), RunBackgroundJobArgs{Cmd: "sleep 30"}, tools.NopRuntime{})
	require.NoError(t, err)

	var jobID string
	tool.handler.jobs.Range(func(id string, _ *backgroundJob) bool {
		jobID = id
		return false
	})
	require.NotEmpty(t, jobID)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	result, err := tool.handler.WaitBackgroundJob(ctx, WaitBackgroundJobArgs{JobID: jobID, Timeout: 60})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "cancelled")
}

func TestBackgroundJobsTool_WaitBackgroundJob_Stopped(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("sleep not available on Windows cmd")
	}
	tool := newTestTool(t)
	require.NoError(t, tool.Start(t.Context()))
	t.Cleanup(func() { _ = tool.Stop(t.Context()) })

	_, err := tool.handler.RunBackgroundJob(t.Context(), RunBackgroundJobArgs{Cmd: "sleep 30"}, tools.NopRuntime{})
	require.NoError(t, err)

	var jobID string
	tool.handler.jobs.Range(func(id string, _ *backgroundJob) bool {
		jobID = id
		return false
	})
	require.NotEmpty(t, jobID)

	go func() {
		_, _ = tool.handler.StopBackgroundJob(t.Context(), StopBackgroundJobArgs{JobID: jobID})
	}()

	result, err := tool.handler.WaitBackgroundJob(t.Context(), WaitBackgroundJobArgs{JobID: jobID, Timeout: 10})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "stopped")
	assert.NotContains(t, result.Output, "Exit Code:",
		"stopped jobs should not show an exit code")
}

func TestBackgroundJobsTool_StopEscalatesWhenSIGTERMIgnored(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX signals; skipped on Windows")
	}

	tool := newTestTool(t)
	t.Cleanup(func() { _ = tool.Stop(t.Context()) })
	_, err := tool.handler.RunBackgroundJob(t.Context(), RunBackgroundJobArgs{
		Cmd: `trap '' TERM; echo ready; while :; do sleep 1; done`,
	}, tools.NopRuntime{})
	require.NoError(t, err)

	var job *backgroundJob
	tool.handler.jobs.Range(func(_ string, candidate *backgroundJob) bool {
		job = candidate
		return false
	})
	require.NotNil(t, job)
	require.Eventually(t, func() bool {
		job.outputMu.RLock()
		defer job.outputMu.RUnlock()
		return strings.Contains(job.output.String(), "ready")
	}, time.Second, 10*time.Millisecond)

	started := time.Now()
	result, err := tool.handler.StopBackgroundJob(t.Context(), StopBackgroundJobArgs{JobID: job.id})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Less(t, time.Since(started), 5*time.Second)
	select {
	case <-job.done:
	case <-time.After(time.Second):
		t.Fatal("process survived stop escalation")
	}
	assert.Equal(t, statusStopped, job.status.Load())

	result, err = tool.handler.StopBackgroundJob(t.Context(), StopBackgroundJobArgs{JobID: job.id})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "not running")
}

func TestBackgroundJobsTool_StopWaitsForRealExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX signals; skipped on Windows")
	}

	tool := newTestTool(t)
	t.Cleanup(func() { _ = tool.Stop(t.Context()) })
	_, err := tool.handler.RunBackgroundJob(t.Context(), RunBackgroundJobArgs{
		Cmd: `trap 'sleep 0.1; exit 0' TERM; echo ready; while :; do sleep 1; done`,
	}, tools.NopRuntime{})
	require.NoError(t, err)

	var job *backgroundJob
	tool.handler.jobs.Range(func(_ string, candidate *backgroundJob) bool {
		job = candidate
		return false
	})
	require.NotNil(t, job)
	require.Eventually(t, func() bool {
		job.outputMu.RLock()
		defer job.outputMu.RUnlock()
		return strings.Contains(job.output.String(), "ready")
	}, time.Second, 10*time.Millisecond)

	result, err := tool.handler.StopBackgroundJob(t.Context(), StopBackgroundJobArgs{JobID: job.id})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	select {
	case <-job.done:
	default:
		t.Fatal("stop returned before the command exited")
	}
	assert.Equal(t, statusStopped, job.status.Load())
}

func TestBackgroundJobsTool_RunBackgroundJob(t *testing.T) {
	t.Parallel()
	tool := newTestTool(t)
	require.NoError(t, tool.Start(t.Context()))
	t.Cleanup(func() { _ = tool.Stop(t.Context()) })

	result, err := tool.handler.RunBackgroundJob(t.Context(), RunBackgroundJobArgs{Cmd: "echo test"}, tools.NopRuntime{})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "Background job started with ID:")
}

func TestBackgroundJobsTool_RunBackgroundJobRecall(t *testing.T) {
	t.Parallel()
	tool := newTestTool(t)
	tool.handler.recall = true
	require.NoError(t, tool.Start(t.Context()))
	t.Cleanup(func() { _ = tool.Stop(t.Context()) })

	rt := &recallRuntime{recalls: make(chan string, 1)}
	result, err := tool.handler.RunBackgroundJobWithRecall(t.Context(), RunBackgroundJobRecallArgs{Cmd: "echo recall-output", Recall: true}, rt)
	require.NoError(t, err)
	assert.Contains(t, result.Output, "Recall: requested")

	select {
	case msg := <-rt.recalls:
		assert.Contains(t, msg, "Background job")
		assert.Contains(t, msg, "finished with status completed")
		assert.Contains(t, msg, "recall-output")
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for background job recall")
	}
}

func TestBackgroundJobsTool_RunBackgroundJobRecallRequiresConfig(t *testing.T) {
	t.Parallel()
	tool := newTestTool(t)

	result, err := tool.handler.RunBackgroundJobWithRecall(t.Context(), RunBackgroundJobRecallArgs{Cmd: "echo test", Recall: true}, &recallRuntime{})
	require.NoError(t, err)
	require.True(t, result.IsError)
	assert.Contains(t, result.Output, "recall")
	assert.Contains(t, result.Output, "not enabled")
	assert.Equal(t, 0, tool.handler.jobs.Length())
}

func TestBackgroundJobsTool_RunBackgroundJobRecallRequiresCallback(t *testing.T) {
	t.Parallel()
	tool := newTestTool(t)
	tool.handler.recall = true

	result, err := tool.handler.RunBackgroundJobWithRecall(t.Context(), RunBackgroundJobRecallArgs{Cmd: "echo test", Recall: true}, tools.NopRuntime{})
	require.NoError(t, err)
	require.True(t, result.IsError)
	assert.Contains(t, result.Output, "does not support recall")
	assert.Equal(t, 0, tool.handler.jobs.Length())
}

type recallRuntime struct {
	tools.NopRuntime

	recalls chan string
}

func (r *recallRuntime) Recall(_ context.Context, message string) error {
	r.recalls <- message
	return nil
}

func (r *recallRuntime) Supports(c tools.Capability) bool {
	return c == tools.CapabilityRecall
}

func TestCreateToolSetEnablesRecall(t *testing.T) {
	t.Parallel()

	recall := true
	toolSet, err := CreateToolSet(t.Context(), latest.Toolset{Type: "background_jobs", Recall: &recall}, &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}})
	require.NoError(t, err)

	backgroundJobsToolSet, ok := toolSet.(*ToolSet)
	require.True(t, ok)
	assert.True(t, backgroundJobsToolSet.handler.recall)
}

func TestBackgroundJobsTool_RunBackgroundJobSchemaRecall(t *testing.T) {
	t.Parallel()

	withoutRecall := newTestTool(t)
	withoutTools, err := withoutRecall.Tools(t.Context())
	require.NoError(t, err)
	withoutSchema, err := tools.SchemaToMap(findTool(t, withoutTools, ToolNameRunBackgroundJob).Parameters)
	require.NoError(t, err)
	withoutProps, ok := withoutSchema["properties"].(map[string]any)
	require.True(t, ok)
	assert.NotContains(t, withoutProps, "recall")

	withRecall := newTestTool(t)
	withRecall.handler.recall = true
	withTools, err := withRecall.Tools(t.Context())
	require.NoError(t, err)
	withSchema, err := tools.SchemaToMap(findTool(t, withTools, ToolNameRunBackgroundJob).Parameters)
	require.NoError(t, err)
	withProps, ok := withSchema["properties"].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, withProps, "recall")
}

func findTool(t *testing.T, toolList []tools.Tool, name string) tools.Tool {
	t.Helper()
	for _, tool := range toolList {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("tool %q not found", name)
	return tools.Tool{}
}

func TestBackgroundJobsTool_ListBackgroundJobs(t *testing.T) {
	t.Parallel()
	tool := newTestTool(t)
	require.NoError(t, tool.Start(t.Context()))
	t.Cleanup(func() { _ = tool.Stop(t.Context()) })

	_, err := tool.handler.RunBackgroundJob(t.Context(), RunBackgroundJobArgs{Cmd: "echo test"}, tools.NopRuntime{})
	require.NoError(t, err)

	listResult, err := tool.handler.ListBackgroundJobs(t.Context(), nil)

	require.NoError(t, err)
	assert.Contains(t, listResult.Output, "Background Jobs:")
	assert.Contains(t, listResult.Output, "ID: job_")
}

func TestBackgroundJobsTool_Instructions(t *testing.T) {
	t.Parallel()

	tool := newTestTool(t)

	instructions := tool.Instructions()

	assert.Contains(t, instructions, "Background Job Tools")
	assert.Contains(t, instructions, "run_background_job")
}

func TestResolveWorkDir(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	absolute := t.TempDir()
	h := &backgroundJobsHandler{workingDir: workingDir}

	tests := []struct {
		name     string
		cwd      string
		expected string
	}{
		{name: "empty defaults to workingDir", cwd: "", expected: workingDir},
		{name: "dot defaults to workingDir", cwd: ".", expected: workingDir},
		{name: "absolute path unchanged", cwd: absolute, expected: absolute},
		{name: "relative path joined with workingDir", cwd: "src/pkg", expected: filepath.Join(workingDir, "src", "pkg")},
		{name: "relative with dot prefix", cwd: "./subdir", expected: filepath.Join(workingDir, "subdir")},
		{name: "relative with parent traversal", cwd: "../sibling", expected: filepath.Join(filepath.Dir(workingDir), "sibling")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, h.resolveWorkDir(tt.cwd))
		})
	}
}

// Regression test for a backgroundjobs hang caused by backgrounded grandchildren.
//
// A command like `sleep 10 &` makes the shell exit immediately, but the
// backgrounded sleep inherits stdout/stderr. Without cmd.WaitDelay, Go's
// exec.Cmd.Wait() blocks reading the pipe until the child dies.
//
// With the WaitDelay safeguard the monitor goroutine must finish within a small
// fraction of the sleep timeout, and correctly interpret ErrWaitDelay as a
// success if the direct child exited 0.
func TestBackgroundJobsTool_BackgroundedChildDoesNotBlockReturn(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell backgrounding semantics; skipped on Windows")
	}

	tool := New(nil, &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}})

	start := time.Now()
	result, err := tool.handler.RunBackgroundJob(t.Context(), RunBackgroundJobArgs{
		// sleep inherits stdout/stderr from the shell and holds the pipe
		// open for 30s. The monitor must return as soon as the shell exits + waitDelay.
		Cmd: "sleep 30 &",
	}, nil)

	require.NoError(t, err)
	require.NotNil(t, result)

	// Wait for the monitor goroutine to mark the job as finished.
	require.Eventually(t, func() bool {
		listResult, err := tool.handler.ListBackgroundJobs(t.Context(), nil)
		require.NoError(t, err)
		return !strings.Contains(listResult.Output, "Status: running")
	}, 10*time.Second, 100*time.Millisecond)

	elapsed := time.Since(start)
	assert.Less(t, elapsed, 5*time.Second,
		"background job monitor must complete promptly when the command daemonizes a child; elapsed=%s", elapsed)

	// Since the direct child (shell) exited 0, the job should be marked completed
	// even though the pipes were forcefully closed (ErrWaitDelay).
	listResult, err := tool.handler.ListBackgroundJobs(t.Context(), nil)
	require.NoError(t, err)
	assert.Contains(t, listResult.Output, "Status: completed")
	assert.Contains(t, listResult.Output, "Exit Code: 0")
}
