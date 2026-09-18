package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

func decodeWaitResult(t *testing.T, result *tools.ToolCallResult, err error) waitResult {
	t.Helper()
	require.NoError(t, err)
	var out waitResult
	require.NoError(t, json.Unmarshal([]byte(result.Output), &out))
	return out
}

func TestHandleWait_InvalidArguments(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args WaitBackgroundAgentsArgs
		want string
	}{
		{name: "empty", want: "task_ids must contain"},
		{name: "too many", args: WaitBackgroundAgentsArgs{TaskIDs: make([]string, maxTotalTasks+1)}, want: "task_ids must contain"},
		{name: "blank ID", args: WaitBackgroundAgentsArgs{TaskIDs: []string{" "}}, want: "must not be empty"},
		{name: "duplicate", args: WaitBackgroundAgentsArgs{TaskIDs: []string{"a", "a"}}, want: "duplicate task ID"},
		{name: "negative timeout", args: WaitBackgroundAgentsArgs{TaskIDs: []string{"a"}, Timeout: -1}, want: "timeout must be between"},
		{name: "excessive timeout", args: WaitBackgroundAgentsArgs{TaskIDs: []string{"a"}, Timeout: maxWaitTimeout + 1}, want: "timeout must be between"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			result, err := newTestHandler().HandleWait(t.Context(), nil, makeToolCall(t, tc.args))
			require.NoError(t, err)
			assert.True(t, result.IsError)
			assert.Contains(t, result.Output, tc.want)
		})
	}
	t.Run("invalid JSON", func(t *testing.T) {
		_, err := newTestHandler().HandleWait(t.Context(), nil, tools.ToolCall{Function: tools.FunctionCall{Arguments: "not-json"}})
		require.Error(t, err)
	})
}

func TestHandleWait_AlreadyFinished(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	insertTask(h, "success", "sub", taskCompleted).result = "finished work"
	insertTask(h, "failure", "sub", taskFailed).errMsg = "model unavailable"
	insertTask(h, "stopped", "sub", taskStopped)
	insertTask(h, "empty", "sub", taskCompleted)
	call := makeToolCall(t, WaitBackgroundAgentsArgs{TaskIDs: []string{"failure", "success", "stopped", "empty"}})

	for range 2 {
		result, err := h.HandleWait(t.Context(), nil, call)
		out := decodeWaitResult(t, result, err)
		assert.False(t, result.IsError)
		assert.True(t, out.AllDone)
		assert.False(t, out.TimedOut)
		assert.Equal(t, []waitTaskResult{
			{TaskID: "failure", Status: "failed", Done: true, Error: "model unavailable"},
			{TaskID: "success", Status: "completed", Done: true, Output: "finished work"},
			{TaskID: "stopped", Status: "stopped", Done: true},
			{TaskID: "empty", Status: "completed", Done: true},
		}, out.Tasks)
	}
}

type waitRunner struct {
	mockRunner

	run func(context.Context, RunParams) *RunResult
}

func (r *waitRunner) RunAgent(ctx context.Context, params RunParams) *RunResult {
	return r.run(ctx, params)
}

func startWaitTask(t *testing.T, h *Handler, name string) string {
	t.Helper()
	result, err := h.HandleRun(t.Context(), session.New(), makeToolCall(t, RunBackgroundAgentArgs{Agent: "sub", Task: name}))
	require.NoError(t, err)
	require.False(t, result.IsError)
	line, _, _ := strings.Cut(result.Output, "\n")
	return strings.TrimPrefix(line, "Background agent task started with ID: ")
}

func TestHandleWait_JoinsAllSelectedTasks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := NewHandler(&waitRunner{
			mockRunner: mockRunner{subAgentNames: []string{"sub"}},
			run: func(ctx context.Context, params RunParams) *RunResult {
				switch params.Task {
				case "success":
					<-time.After(3 * time.Second)
					return &RunResult{Result: "finished work"}
				case "failure":
					<-time.After(time.Second)
					return &RunResult{ErrMsg: "model unavailable"}
				default:
					<-ctx.Done()
					return &RunResult{}
				}
			},
		})
		defer h.StopAll()
		failure := startWaitTask(t, h, "failure")
		success := startWaitTask(t, h, "success")
		unrelated := startWaitTask(t, h, "unrelated")
		start := time.Now()
		result, err := h.HandleWait(t.Context(), nil, makeToolCall(t, WaitBackgroundAgentsArgs{TaskIDs: []string{failure, success}}))
		out := decodeWaitResult(t, result, err)
		assert.False(t, result.IsError)
		assert.True(t, out.AllDone)
		assert.Equal(t, 3*time.Second, time.Since(start))
		assert.Equal(t, "model unavailable", out.Tasks[0].Error)
		assert.Equal(t, "finished work", out.Tasks[1].Output)
		task, ok := h.tasks.Load(unrelated)
		require.True(t, ok)
		assert.Equal(t, taskRunning, task.loadStatus())
	})
}

func TestHandleWait_Timeout(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout int
		want    time.Duration
	}{
		{name: "default", want: defaultWaitTimeout * time.Second},
		{name: "explicit", timeout: 5, want: 5 * time.Second},
		{name: "maximum", timeout: maxWaitTimeout, want: maxWaitTimeout * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				release := make(chan struct{})
				h := NewHandler(&waitRunner{
					mockRunner: mockRunner{subAgentNames: []string{"sub"}},
					run: func(ctx context.Context, params RunParams) *RunResult {
						if params.Task == "fast" {
							<-time.After(time.Second)
							return &RunResult{Result: "fast result"}
						}
						params.OnContent("working")
						select {
						case <-release:
						case <-ctx.Done():
							t.Error("wait must not cancel workers")
						}
						return &RunResult{Result: "slow result"}
					},
				})
				defer h.StopAll()
				fast := startWaitTask(t, h, "fast")
				slow := startWaitTask(t, h, "slow")
				call := makeToolCall(t, WaitBackgroundAgentsArgs{TaskIDs: []string{fast, slow}, Timeout: tc.timeout})
				start := time.Now()
				result, err := h.HandleWait(t.Context(), nil, call)
				out := decodeWaitResult(t, result, err)
				assert.False(t, result.IsError)
				assert.False(t, out.AllDone)
				assert.True(t, out.TimedOut)
				assert.Equal(t, tc.want, time.Since(start), "the timeout is shared, not per task")
				assert.True(t, out.Tasks[0].Done)
				assert.Equal(t, "working", out.Tasks[1].Output)
				assert.Equal(t, "running", out.Tasks[1].Status)
				assert.False(t, out.Tasks[1].Done)

				close(release)
				result, err = h.HandleWait(t.Context(), nil, call)
				out = decodeWaitResult(t, result, err)
				assert.True(t, out.AllDone)
				assert.False(t, out.TimedOut)
				assert.Equal(t, "slow result", out.Tasks[1].Output)
			})
		})
	}
}

func TestHandleWait_Cancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		h := NewHandler(&blockingRunner{release: release})
		defer h.StopAll()
		id := startWaitTask(t, h, "work")
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		call := makeToolCall(t, WaitBackgroundAgentsArgs{TaskIDs: []string{id}})
		result, err := h.HandleWait(ctx, nil, call)
		out := decodeWaitResult(t, result, err)
		assert.True(t, result.IsError)
		assert.False(t, out.AllDone)
		assert.False(t, out.TimedOut)
		assert.Contains(t, out.Error, "Wait cancelled")
		assert.Equal(t, "running", out.Tasks[0].Status)

		close(release)
		result, err = h.HandleWait(t.Context(), nil, call)
		out = decodeWaitResult(t, result, err)
		assert.True(t, out.AllDone)
		assert.Equal(t, "completed", out.Tasks[0].Status)
	})
}

func TestHandleWait_StopWaitsForExecutionToExit(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(map[bool]string{false: "stop task", true: "shutdown"}[shutdown], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := NewHandler(&waitRunner{
					mockRunner: mockRunner{subAgentNames: []string{"sub"}},
					run: func(ctx context.Context, _ RunParams) *RunResult {
						<-ctx.Done()
						<-time.After(2 * time.Second)
						return &RunResult{}
					},
				})
				defer h.StopAll()
				id := startWaitTask(t, h, "work")
				if shutdown {
					go h.StopAll()
				} else {
					result, err := h.HandleStop(t.Context(), nil, makeToolCall(t, StopBackgroundAgentArgs{TaskID: id}))
					require.NoError(t, err)
					require.False(t, result.IsError)
				}
				synctest.Wait()
				h.pruneCompleted()
				require.Equal(t, 1, h.totalTaskCount(), "stopping tasks must remain joinable")
				start := time.Now()
				call := makeToolCall(t, WaitBackgroundAgentsArgs{TaskIDs: []string{id}, Timeout: 1})
				result, err := h.HandleWait(t.Context(), nil, call)
				out := decodeWaitResult(t, result, err)
				assert.False(t, out.AllDone)
				assert.True(t, out.TimedOut)
				assert.Equal(t, "stopped", out.Tasks[0].Status)
				assert.False(t, out.Tasks[0].Done)

				call = makeToolCall(t, WaitBackgroundAgentsArgs{TaskIDs: []string{id}})
				result, err = h.HandleWait(t.Context(), nil, call)
				out = decodeWaitResult(t, result, err)
				assert.True(t, out.AllDone)
				assert.Equal(t, 2*time.Second, time.Since(start))
				assert.True(t, out.Tasks[0].Done)
			})
		})
	}
}

func TestHandleWait_ConcurrentWaitersAndPruning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newTestHandler()
		task := insertTask(h, "work", "sub", taskRunning)
		call := makeToolCall(t, WaitBackgroundAgentsArgs{TaskIDs: []string{"work"}})
		results := make(chan *tools.ToolCallResult, 2)
		for range 2 {
			go func() {
				result, err := h.HandleWait(t.Context(), nil, call)
				assert.NoError(t, err)
				results <- result
			}()
		}
		synctest.Wait()
		task.result = "finished"
		task.storeStatus(taskCompleted)
		close(task.done)
		h.pruneCompleted()
		assert.Zero(t, h.totalTaskCount())
		for range 2 {
			out := decodeWaitResult(t, <-results, nil)
			assert.True(t, out.AllDone)
			assert.Equal(t, "finished", out.Tasks[0].Output)
		}
	})
}

func TestHandleWait_UnknownAndPrunedTasks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newTestHandler()
		insertTask(h, "pruned", "sub", taskCompleted)
		h.pruneCompleted()
		task := insertTask(h, "work", "sub", taskRunning)
		go func() {
			<-time.After(time.Second)
			task.result = "finished"
			task.storeStatus(taskCompleted)
			close(task.done)
		}()
		result, err := h.HandleWait(t.Context(), nil, makeToolCall(t, WaitBackgroundAgentsArgs{TaskIDs: []string{"unknown", "work", "pruned"}}))
		out := decodeWaitResult(t, result, err)
		assert.True(t, result.IsError)
		assert.False(t, out.AllDone)
		assert.Equal(t, "not_found", out.Tasks[0].Status)
		assert.False(t, out.Tasks[0].Done)
		assert.Equal(t, "finished", out.Tasks[1].Output)
		assert.True(t, out.Tasks[1].Done)
		assert.Equal(t, "not_found", out.Tasks[2].Status)
	})
}

func TestHandleWait_BoundedOutput(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	long := strings.Repeat("界", maxWaitOutputBytes)
	insertTask(h, "success", "sub", taskCompleted).result = long
	insertTask(h, "failure", "sub", taskFailed).errMsg = long
	insertTask(h, "running", "sub", taskRunning).writeOutput(long)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result, err := h.HandleWait(ctx, nil, makeToolCall(t, WaitBackgroundAgentsArgs{TaskIDs: []string{"success", "failure", "running"}}))
	out := decodeWaitResult(t, result, err)
	for _, entry := range out.Tasks {
		assert.True(t, entry.Truncated)
		assert.LessOrEqual(t, len(entry.Output)+len(entry.Error), maxWaitOutputBytes)
		assert.True(t, utf8.ValidString(entry.Output+entry.Error))
	}
	result, err = h.HandleView(t.Context(), nil, makeToolCall(t, ViewBackgroundAgentArgs{TaskID: "success"}))
	require.NoError(t, err)
	assert.Contains(t, result.Output, long, "waiting must not truncate the stored result")
}
