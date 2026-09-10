package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/concurrent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// mockRunner implements Runner plus the optional SessionSubAgentResolver,
// mirroring production LocalRuntime which implements both.
type mockRunner struct {
	subAgentNames []string
	runResult     *RunResult
	runDelay      time.Duration // optional delay to simulate work

	mu            sync.Mutex
	validatedSess *session.Session // session SubAgentNames resolved against
	legacyCalls   int              // CurrentAgentSubAgentNames invocations
}

func (m *mockRunner) CurrentAgentSubAgentNames() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.legacyCalls++
	return m.subAgentNames
}

func (m *mockRunner) SubAgentNames(sess *session.Session) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.validatedSess = sess
	return m.subAgentNames
}

func (m *mockRunner) RunAgent(ctx context.Context, params RunParams) *RunResult {
	if m.runDelay > 0 {
		select {
		case <-time.After(m.runDelay):
		case <-ctx.Done():
			return &RunResult{}
		}
	}
	// Call OnContent if result has content, to simulate streaming.
	if m.runResult != nil && m.runResult.Result != "" && params.OnContent != nil {
		params.OnContent(m.runResult.Result)
	}
	if m.runResult != nil {
		return m.runResult
	}
	return &RunResult{}
}

// legacyRunner implements only the base Runner contract — no
// SessionSubAgentResolver — standing in for out-of-tree Runner
// implementations that predate the session-aware resolver.
type legacyRunner struct {
	subAgentNames []string
	runResult     *RunResult
}

func (m *legacyRunner) CurrentAgentSubAgentNames() []string {
	return m.subAgentNames
}

func (m *legacyRunner) RunAgent(context.Context, RunParams) *RunResult {
	if m.runResult != nil {
		return m.runResult
	}
	return &RunResult{}
}

func newTestHandler() *Handler {
	return &Handler{
		tasks: concurrent.NewMap[string, *task](),
	}
}

func newTestHandlerWithRunner(r Runner) *Handler {
	return NewHandler(r)
}

func insertTask(h *Handler, id, agentName string, status taskStatus) *task {
	h.admissionMu.Lock()
	defer h.admissionMu.Unlock()

	t := &task{
		id:        id,
		agentName: agentName,
		taskDesc:  "test task",
		cancel:    func() {},
		startTime: time.Now(),
	}
	t.status.Store(int32(status))
	if status == taskRunning {
		h.activeTasks++
	}
	h.tasks.Store(id, t)
	return t
}

func makeToolCall(t *testing.T, args any) tools.ToolCall {
	t.Helper()
	b, err := json.Marshal(args)
	require.NoError(t, err)
	return tools.ToolCall{Function: tools.FunctionCall{Arguments: string(b)}}
}

// --- newTaskID ---

func TestNewTaskID_IsUnique(t *testing.T) {
	t.Parallel()
	ids := make(map[string]struct{})
	for range 100 {
		id := newTaskID()
		assert.NotEmpty(t, id)
		_, dup := ids[id]
		assert.False(t, dup, "duplicate task ID: %s", id)
		ids[id] = struct{}{}
	}
}

func TestNewTaskID_HasPrefix(t *testing.T) {
	t.Parallel()
	id := newTaskID()
	assert.True(t, strings.HasPrefix(id, "agent_task_"), "ID should start with agent_task_ prefix, got: %s", id)
}

// --- statusToString ---

func TestStatusToString(t *testing.T) {
	t.Parallel()
	cases := []struct {
		status   taskStatus
		expected string
	}{
		{taskRunning, "running"},
		{taskCompleted, "completed"},
		{taskStopped, "stopped"},
		{taskFailed, "failed"},
		{99, "unknown"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.expected, tc.status.String())
	}
}

// --- runningTaskCount / totalTaskCount ---

func TestTaskCounts(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	assert.Equal(t, 0, h.runningTaskCount())
	assert.Equal(t, 0, h.totalTaskCount())

	insertTask(h, "t1", "a", taskRunning)
	insertTask(h, "t2", "b", taskRunning)
	insertTask(h, "t3", "c", taskCompleted)
	insertTask(h, "t4", "d", taskFailed)

	assert.Equal(t, 2, h.runningTaskCount())
	assert.Equal(t, 4, h.totalTaskCount())
}

// --- pruneCompleted ---

func TestPruneCompleted(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	insertTask(h, "run1", "a", taskRunning)
	insertTask(h, "done1", "b", taskCompleted)
	insertTask(h, "done2", "c", taskStopped)
	insertTask(h, "fail1", "d", taskFailed)

	h.pruneCompleted()

	assert.Equal(t, 1, h.totalTaskCount())
	_, exists := h.tasks.Load("run1")
	assert.True(t, exists, "running task should be kept")
	_, exists = h.tasks.Load("done1")
	assert.False(t, exists, "completed task should be pruned")
}

// --- HandleList ---

func TestHandleList_Empty(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	result, err := h.HandleList(t.Context(), nil, tools.ToolCall{})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "No background agent tasks found")
}

func TestHandleList_ShowsTasks(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	insertTask(h, "t1", "researcher", taskRunning)
	insertTask(h, "t2", "writer", taskCompleted)

	result, err := h.HandleList(t.Context(), nil, tools.ToolCall{})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "researcher")
	assert.Contains(t, result.Output, "writer")
	assert.Contains(t, result.Output, "running")
	assert.Contains(t, result.Output, "completed")
}

// --- HandleView ---

func TestHandleView_NotFound(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	tc := makeToolCall(t, ViewBackgroundAgentArgs{TaskID: "nonexistent"})
	result, err := h.HandleView(t.Context(), nil, tc)
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "task not found")
}

func TestHandleView_Completed(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	tk := insertTask(h, "t1", "researcher", taskCompleted)
	tk.result = "Here is my research."

	tc := makeToolCall(t, ViewBackgroundAgentArgs{TaskID: "t1"})
	result, err := h.HandleView(t.Context(), nil, tc)
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Output, "Here is my research.")
	assert.Contains(t, result.Output, "completed")
}

func TestHandleView_Failed(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	tk := insertTask(h, "t1", "researcher", taskFailed)
	tk.errMsg = "model unavailable"

	tc := makeToolCall(t, ViewBackgroundAgentArgs{TaskID: "t1"})
	result, err := h.HandleView(t.Context(), nil, tc)
	require.NoError(t, err)
	assert.Contains(t, result.Output, "task failed")
	assert.Contains(t, result.Output, "model unavailable")
}

func TestHandleView_Running_NoOutputYet(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	insertTask(h, "t1", "researcher", taskRunning)

	tc := makeToolCall(t, ViewBackgroundAgentArgs{TaskID: "t1"})
	result, err := h.HandleView(t.Context(), nil, tc)
	require.NoError(t, err)
	assert.Contains(t, result.Output, "no output yet")
}

func TestHandleView_Running_WithProgress(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	tk := insertTask(h, "t1", "researcher", taskRunning)
	tk.output.WriteString("Partial research so far...")

	tc := makeToolCall(t, ViewBackgroundAgentArgs{TaskID: "t1"})
	result, err := h.HandleView(t.Context(), nil, tc)
	require.NoError(t, err)
	assert.Contains(t, result.Output, "Partial research so far...")
	assert.Contains(t, result.Output, "still running")
}

func TestHandleView_Stopped(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	insertTask(h, "t1", "researcher", taskStopped)

	tc := makeToolCall(t, ViewBackgroundAgentArgs{TaskID: "t1"})
	result, err := h.HandleView(t.Context(), nil, tc)
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Output, "stopped")
	assert.Contains(t, result.Output, "task was stopped")
}

func TestHandleView_Completed_EmptyResult(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	insertTask(h, "t1", "researcher", taskCompleted)

	tc := makeToolCall(t, ViewBackgroundAgentArgs{TaskID: "t1"})
	result, err := h.HandleView(t.Context(), nil, tc)
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Output, "no output")
}

func TestHandleView_OutputBufferTruncated(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	tk := insertTask(h, "t1", "researcher", taskRunning)
	tk.output.WriteString(strings.Repeat("x", maxOutputBytes))
	tk.outputBytes = maxOutputBytes

	tc := makeToolCall(t, ViewBackgroundAgentArgs{TaskID: "t1"})
	result, err := h.HandleView(t.Context(), nil, tc)
	require.NoError(t, err)
	assert.Contains(t, result.Output, "truncated", "should show truncation notice when buffer is full")
	assert.Contains(t, result.Output, "still running")
}

func TestHandleView_InvalidJSON(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	bad := tools.ToolCall{Function: tools.FunctionCall{Arguments: "not-json"}}
	_, err := h.HandleView(t.Context(), nil, bad)
	require.Error(t, err, "invalid JSON should return an error")
}

func TestHandleView_RepeatedPolling_NoNewOutput(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	insertTask(h, "t1", "researcher", taskRunning)

	tc := makeToolCall(t, ViewBackgroundAgentArgs{TaskID: "t1"})

	// First view should not include poll marker.
	result1, err := h.HandleView(t.Context(), nil, tc)
	require.NoError(t, err)
	assert.NotContains(t, result1.Output, "poll #")

	// Second view with no new output should include poll marker.
	result2, err := h.HandleView(t.Context(), nil, tc)
	require.NoError(t, err)
	assert.Contains(t, result2.Output, "poll #2")

	// Third view should show poll #3.
	result3, err := h.HandleView(t.Context(), nil, tc)
	require.NoError(t, err)
	assert.Contains(t, result3.Output, "poll #3")

	// Responses should be non-identical.
	assert.NotEqual(t, result2.Output, result3.Output)
}

func TestHandleView_RepeatedPolling_OutputGrows(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	tk := insertTask(h, "t1", "researcher", taskRunning)

	tc := makeToolCall(t, ViewBackgroundAgentArgs{TaskID: "t1"})

	// First view.
	_, err := h.HandleView(t.Context(), nil, tc)
	require.NoError(t, err)

	// Second view with no change → poll #2.
	result2, err := h.HandleView(t.Context(), nil, tc)
	require.NoError(t, err)
	assert.Contains(t, result2.Output, "poll #2")

	// Simulate new output arriving.
	tk.outputMu.Lock()
	tk.output.WriteString("new progress")
	tk.outputBytes += len("new progress")
	tk.outputMu.Unlock()

	// Third view should reset the poll counter since output changed.
	result3, err := h.HandleView(t.Context(), nil, tc)
	require.NoError(t, err)
	assert.NotContains(t, result3.Output, "poll #", "poll marker should reset after new output")
	assert.Contains(t, result3.Output, "new progress")
}

// --- HandleStop ---

func TestHandleStop_NotFound(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	tc := makeToolCall(t, StopBackgroundAgentArgs{TaskID: "ghost"})
	result, err := h.HandleStop(t.Context(), nil, tc)
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "task not found")
}

func TestHandleStop_AlreadyCompleted(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	insertTask(h, "t1", "researcher", taskCompleted)

	tc := makeToolCall(t, StopBackgroundAgentArgs{TaskID: "t1"})
	result, err := h.HandleStop(t.Context(), nil, tc)
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "not running")
}

func TestHandleStop_Running(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	cancelled := false
	tk := insertTask(h, "t1", "researcher", taskRunning)
	tk.cancel = func() { cancelled = true }

	tc := makeToolCall(t, StopBackgroundAgentArgs{TaskID: "t1"})
	result, err := h.HandleStop(t.Context(), nil, tc)
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.True(t, cancelled)
	assert.Equal(t, taskStopped, tk.loadStatus())
}

func TestHandleStop_InvalidJSON(t *testing.T) {
	t.Parallel()
	h := newTestHandler()
	bad := tools.ToolCall{Function: tools.FunctionCall{Arguments: "not-json"}}
	_, err := h.HandleStop(t.Context(), nil, bad)
	require.Error(t, err, "invalid JSON should return an error")
}

// --- StopAll waits for goroutines ---

func TestStopAll_WaitsForGoroutines(t *testing.T) {
	t.Parallel()
	h := newTestHandler()

	var goroutineExited atomic.Bool
	tk := insertTask(h, "t1", "researcher", taskRunning)
	ctx, cancel := context.WithCancel(t.Context())
	tk.cancel = cancel

	// The goroutine only finishes once the test releases it, so StopAll
	// returning proves it waited rather than raced ahead.
	release := make(chan struct{})
	h.wg.Go(func() {
		<-ctx.Done()
		<-release
		goroutineExited.Store(true)
	})

	stopDone := make(chan struct{})
	go func() {
		h.StopAll()
		close(stopDone)
	}()

	<-ctx.Done() // StopAll has cancelled the task
	close(release)
	<-stopDone
	assert.True(t, goroutineExited.Load(), "StopAll should wait for goroutine to exit")
}

// --- HandleRun: input validation ---

func TestHandleRun_EmptyAgent(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithRunner(&mockRunner{subAgentNames: []string{"sub"}})
	tc := makeToolCall(t, RunBackgroundAgentArgs{Agent: "", Task: "do something"})
	result, err := h.HandleRun(t.Context(), session.New(), tc)
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "agent name must not be empty")
}

func TestHandleRun_EmptyTask(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithRunner(&mockRunner{subAgentNames: []string{"sub"}})
	tc := makeToolCall(t, RunBackgroundAgentArgs{Agent: "sub", Task: ""})
	result, err := h.HandleRun(t.Context(), session.New(), tc)
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "task must not be empty")
}

func TestHandleRun_InvalidSubAgent(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithRunner(&mockRunner{subAgentNames: []string{"sub"}})
	tc := makeToolCall(t, RunBackgroundAgentArgs{Agent: "nonexistent", Task: "do something"})
	result, err := h.HandleRun(t.Context(), session.New(), tc)
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "not in the sub-agents list")
}

func TestHandleRun_NoSubAgents(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithRunner(&mockRunner{subAgentNames: nil})
	tc := makeToolCall(t, RunBackgroundAgentArgs{Agent: "some-agent", Task: "do something"})
	result, err := h.HandleRun(t.Context(), session.New(), tc)
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "no sub-agents configured")
}

func TestHandleRun_ConcurrencyCapEnforced(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithRunner(&mockRunner{subAgentNames: []string{"sub"}})

	for i := range maxConcurrentTasks {
		insertTask(h, "fake"+string(rune('a'+i)), "sub", taskRunning)
	}

	tc := makeToolCall(t, RunBackgroundAgentArgs{Agent: "sub", Task: "do something"})
	result, err := h.HandleRun(t.Context(), session.New(), tc)
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "maximum concurrent")
}

type blockingRunner struct {
	started   atomic.Int32
	active    atomic.Int32
	maxActive atomic.Int32
	release   chan struct{}
}

func (*blockingRunner) CurrentAgentSubAgentNames() []string { return []string{"sub"} }

func (r *blockingRunner) RunAgent(ctx context.Context, _ RunParams) *RunResult {
	r.started.Add(1)
	active := r.active.Add(1)
	defer r.active.Add(-1)
	for {
		maxActive := r.maxActive.Load()
		if active <= maxActive || r.maxActive.CompareAndSwap(maxActive, active) {
			break
		}
	}
	select {
	case <-r.release:
	case <-ctx.Done():
	}
	return &RunResult{}
}

func TestHandleRun_ConcurrentAdmissionEnforcesCap(t *testing.T) {
	t.Parallel()

	runner := &blockingRunner{release: make(chan struct{})}
	h := newTestHandlerWithRunner(runner)
	t.Cleanup(h.StopAll)
	tc := makeToolCall(t, RunBackgroundAgentArgs{Agent: "sub", Task: "work"})

	start := make(chan struct{})
	results := make(chan *tools.ToolCallResult, 2*maxConcurrentTasks)
	var calls sync.WaitGroup
	for range 2 * maxConcurrentTasks {
		calls.Go(func() {
			<-start
			result, err := h.HandleRun(t.Context(), session.New(), tc)
			require.NoError(t, err)
			results <- result
		})
	}
	close(start)
	calls.Wait()
	close(results)

	var admitted int
	for result := range results {
		if !result.IsError {
			admitted++
		}
	}
	assert.Equal(t, maxConcurrentTasks, admitted)
	require.Eventually(t, func() bool {
		return runner.started.Load() == maxConcurrentTasks
	}, time.Second, time.Millisecond)
	assert.LessOrEqual(t, runner.maxActive.Load(), int32(maxConcurrentTasks))
	assert.Equal(t, maxConcurrentTasks, h.totalTaskCount())

	close(runner.release)
	h.wg.Wait()
}

func TestHandleRun_RejectsAdmissionDuringShutdown(t *testing.T) {
	t.Parallel()

	runner := &blockingRunner{release: make(chan struct{})}
	h := newTestHandlerWithRunner(runner)
	tc := makeToolCall(t, RunBackgroundAgentArgs{Agent: "sub", Task: "work"})

	first, err := h.HandleRun(t.Context(), session.New(), tc)
	require.NoError(t, err)
	require.False(t, first.IsError)
	require.Eventually(t, func() bool { return runner.started.Load() == 1 }, time.Second, time.Millisecond)

	stopped := make(chan struct{})
	go func() {
		h.StopAll()
		close(stopped)
	}()

	require.Eventually(t, func() bool {
		h.admissionMu.Lock()
		defer h.admissionMu.Unlock()
		return h.stopping
	}, time.Second, time.Millisecond)

	result, err := h.HandleRun(t.Context(), session.New(), tc)
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "stopping")
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("StopAll did not finish")
	}
	assert.Equal(t, int32(1), runner.started.Load())
}

func TestHandleRun_InvalidJSON(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithRunner(&mockRunner{subAgentNames: []string{"sub"}})
	bad := tools.ToolCall{Function: tools.FunctionCall{Arguments: "not-json"}}
	_, err := h.HandleRun(t.Context(), session.New(), bad)
	require.Error(t, err, "invalid JSON should return an error")
}

func TestHandleRun_StartsTask(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithRunner(&mockRunner{
		subAgentNames: []string{"sub"},
		runResult:     &RunResult{Result: "done"},
	})

	tc := makeToolCall(t, RunBackgroundAgentArgs{Agent: "sub", Task: "write a poem"})
	result, err := h.HandleRun(t.Context(), session.New(), tc)
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Output, "agent_task_")
	assert.Contains(t, result.Output, "sub")

	h.wg.Wait()

	assert.Equal(t, 1, h.totalTaskCount())
	h.tasks.Range(func(_ string, tk *task) bool {
		assert.Equal(t, taskCompleted, tk.loadStatus())
		return true
	})
}

func TestHandleRun_ProviderError_TaskFails(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithRunner(&mockRunner{
		subAgentNames: []string{"sub"},
		runResult:     &RunResult{ErrMsg: "model unavailable"},
	})

	tc := makeToolCall(t, RunBackgroundAgentArgs{Agent: "sub", Task: "do something"})
	result, err := h.HandleRun(t.Context(), session.New(), tc)
	require.NoError(t, err)
	assert.False(t, result.IsError, "HandleRun should start successfully before provider error")

	h.wg.Wait()

	h.tasks.Range(func(_ string, tk *task) bool {
		assert.Equal(t, taskFailed, tk.loadStatus(), "task should be marked failed on provider error")
		assert.NotEmpty(t, tk.errMsg)
		return true
	})
}

func TestHandleRun_WithExpectedOutput(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithRunner(&mockRunner{
		subAgentNames: []string{"sub"},
		runResult:     &RunResult{Result: "result"},
	})

	tc := makeToolCall(t, RunBackgroundAgentArgs{
		Agent:          "sub",
		Task:           "summarize the document",
		ExpectedOutput: "A one-paragraph summary",
	})
	result, err := h.HandleRun(t.Context(), session.New(), tc)
	require.NoError(t, err)
	assert.False(t, result.IsError)

	h.wg.Wait()

	h.tasks.Range(func(_ string, tk *task) bool {
		assert.Equal(t, taskCompleted, tk.loadStatus())
		return true
	})
}

// TestHandleRun_ValidatesAgainstCallingSession pins the #3886 contract:
// target validation receives the calling session, so the Runner can resolve
// a pinned background caller instead of shared current-agent state.
func TestHandleRun_ValidatesAgainstCallingSession(t *testing.T) {
	t.Parallel()
	m := &mockRunner{subAgentNames: []string{"sub"}, runResult: &RunResult{Result: "done"}}
	h := newTestHandlerWithRunner(m)

	sess := session.New()
	tc := makeToolCall(t, RunBackgroundAgentArgs{Agent: "sub", Task: "do something"})
	result, err := h.HandleRun(t.Context(), sess, tc)
	require.NoError(t, err)
	assert.False(t, result.IsError)

	h.wg.Wait()

	m.mu.Lock()
	defer m.mu.Unlock()
	assert.Same(t, sess, m.validatedSess,
		"HandleRun must validate the target against the calling session")
	assert.Zero(t, m.legacyCalls,
		"the session-aware resolver must be preferred over the legacy method")
}

// TestHandleRun_LegacyRunnerFallback pins source compatibility: a Runner
// without SessionSubAgentResolver still validates and dispatches through
// the legacy CurrentAgentSubAgentNames fallback.
func TestHandleRun_LegacyRunnerFallback(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithRunner(&legacyRunner{
		subAgentNames: []string{"sub"},
		runResult:     &RunResult{Result: "done"},
	})

	// The fallback is the only source of names, so a rejected unknown
	// target proves validation flowed through it.
	bad := makeToolCall(t, RunBackgroundAgentArgs{Agent: "nonexistent", Task: "do something"})
	result, err := h.HandleRun(t.Context(), session.New(), bad)
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "not in the sub-agents list")

	good := makeToolCall(t, RunBackgroundAgentArgs{Agent: "sub", Task: "do something"})
	result, err = h.HandleRun(t.Context(), session.New(), good)
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Output, "agent_task_")

	h.wg.Wait()

	h.tasks.Range(func(_ string, tk *task) bool {
		assert.Equal(t, taskCompleted, tk.loadStatus())
		return true
	})
}

func TestHandleRun_TotalCapAutoPruneAdmits(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithRunner(&mockRunner{
		subAgentNames: []string{"sub"},
		runResult:     &RunResult{Result: "done"},
	})

	for i := range maxTotalTasks {
		insertTask(h, fmt.Sprintf("done%d", i), "sub", taskCompleted)
	}
	assert.Equal(t, maxTotalTasks, h.totalTaskCount())

	tc := makeToolCall(t, RunBackgroundAgentArgs{Agent: "sub", Task: "do something"})
	result, err := h.HandleRun(t.Context(), session.New(), tc)
	require.NoError(t, err)
	assert.False(t, result.IsError, "task should be admitted after auto-prune of completed tasks")

	h.wg.Wait()
}

func TestHandleRun_TotalCapExhaustion_ConcurrencyCapFiresFirst(t *testing.T) {
	t.Parallel()
	h := newTestHandlerWithRunner(&mockRunner{subAgentNames: []string{"sub"}})

	for i := range maxConcurrentTasks {
		insertTask(h, fmt.Sprintf("run%d", i), "sub", taskRunning)
	}

	tc := makeToolCall(t, RunBackgroundAgentArgs{Agent: "sub", Task: "do something"})
	result, err := h.HandleRun(t.Context(), session.New(), tc)
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "maximum concurrent",
		"concurrency cap should fire before total cap can be exhausted non-prunably")
}

// --- Concurrent handler access (run with -race) ---

func TestHandler_ConcurrentAccess(t *testing.T) {
	t.Parallel()
	h := newTestHandler()

	for i := range 10 {
		tk := insertTask(h, fmt.Sprintf("task%d", i), "researcher", taskRunning)
		tk.output.WriteString("some progress output")
		tk.outputBytes = len("some progress output")
	}

	viewTCs := make([]tools.ToolCall, 5)
	for i := range 5 {
		viewTCs[i] = makeToolCall(t, ViewBackgroundAgentArgs{TaskID: fmt.Sprintf("task%d", i%10)})
	}
	stopTCs := make([]tools.ToolCall, 3)
	for i := range 3 {
		stopTCs[i] = makeToolCall(t, StopBackgroundAgentArgs{TaskID: fmt.Sprintf("task%d", i)})
	}

	var wg sync.WaitGroup

	for range 5 {
		wg.Go(func() {
			_, _ = h.HandleList(t.Context(), nil, tools.ToolCall{})
		})
	}

	for i := range 5 {
		wg.Add(1)
		go func(tc tools.ToolCall) {
			defer wg.Done()
			_, _ = h.HandleView(t.Context(), nil, tc)
		}(viewTCs[i])
	}

	for i := range 3 {
		wg.Add(1)
		go func(tc tools.ToolCall) {
			defer wg.Done()
			_, _ = h.HandleStop(t.Context(), nil, tc)
		}(stopTCs[i])
	}

	wg.Wait()
	assert.LessOrEqual(t, h.runningTaskCount(), 10)
}

// --- Tools ---

func TestNewToolSet_ReturnsFourTools(t *testing.T) {
	t.Parallel()
	ts := New()
	toolsList, err := ts.Tools(t.Context())
	require.NoError(t, err)
	assert.Len(t, toolsList, 4)

	names := make([]string, len(toolsList))
	for i, tl := range toolsList {
		names[i] = tl.Name
	}
	assert.Contains(t, names, ToolNameRunBackgroundAgent)
	assert.Contains(t, names, ToolNameListBackgroundAgents)
	assert.Contains(t, names, ToolNameViewBackgroundAgent)
	assert.Contains(t, names, ToolNameStopBackgroundAgent)
}

func TestNewToolSet_Instructions(t *testing.T) {
	t.Parallel()
	ts := New()
	instructable, ok := ts.(tools.Instructable)
	require.True(t, ok, "NewToolSet should implement Instructable")

	instructions := instructable.Instructions()
	assert.NotEmpty(t, instructions)
	assert.Contains(t, instructions, "run_background_agent")
	assert.Contains(t, instructions, "list_background_agents")
	assert.Contains(t, instructions, "view_background_agent")
	assert.Contains(t, instructions, "stop_background_agent")
}
