package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/docker/docker-agent/pkg/concurrent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

const (
	ToolNameRunBackgroundAgent   = "run_background_agent"
	ToolNameListBackgroundAgents = "list_background_agents"
	ToolNameViewBackgroundAgent  = "view_background_agent"
	ToolNameStopBackgroundAgent  = "stop_background_agent"
)

const (
	// maxConcurrentTasks is the maximum number of simultaneously running background agent tasks.
	maxConcurrentTasks = 20
	// maxTotalTasks caps total stored tasks (running + completed) to prevent unbounded memory growth.
	maxTotalTasks = 100
	// maxOutputBytes caps the live output buffer per task, mirroring the shell tool's limit.
	maxOutputBytes = 10 * 1024 * 1024 // 10 MB
)

// RunBackgroundAgentArgs specifies the parameters for dispatching a sub-agent task asynchronously.
type RunBackgroundAgentArgs struct {
	Agent          string `json:"agent" jsonschema:"The name of the sub-agent to run in the background."`
	Task           string `json:"task" jsonschema:"A clear and concise description of the task the agent should achieve."`
	ExpectedOutput string `json:"expected_output,omitempty" jsonschema:"The expected output from the agent (optional)."`
}

// ViewBackgroundAgentArgs specifies the task ID to inspect.
type ViewBackgroundAgentArgs struct {
	TaskID string `json:"task_id" jsonschema:"The ID of the background agent task to view."`
}

// StopBackgroundAgentArgs specifies the task ID to cancel.
type StopBackgroundAgentArgs struct {
	TaskID string `json:"task_id" jsonschema:"The ID of the background agent task to stop."`
}

// RunParams holds the parameters for running a sub-agent.
type RunParams struct {
	AgentName      string
	Task           string
	ExpectedOutput string
	ParentSession  *session.Session
	OnContent      func(content string)
}

// RunResult holds the outcome of a sub-agent execution.
type RunResult struct {
	Result string // final assistant message on completion
	ErrMsg string // error detail if failed
}

// Runner abstracts the runtime dependency for background agent execution.
type Runner interface {
	// CurrentAgentSubAgentNames returns the names of the current agent's sub-agents.
	CurrentAgentSubAgentNames() []string
	// RunAgent starts a sub-agent and blocks until completion or cancellation.
	RunAgent(ctx context.Context, params RunParams) *RunResult
}

// taskStatus represents the lifecycle state of a background agent task.
type taskStatus int32

const (
	taskRunning taskStatus = iota
	taskCompleted
	taskStopped
	taskFailed
)

// String returns a human-readable name for the status.
func (s taskStatus) String() string {
	switch s {
	case taskRunning:
		return "running"
	case taskCompleted:
		return "completed"
	case taskStopped:
		return "stopped"
	case taskFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// task tracks a single background sub-agent execution.
type task struct {
	id        string
	agentName string
	taskDesc  string

	cancel    context.CancelFunc
	startTime time.Time
	status    atomic.Int32
	result    string
	errMsg    string

	// outputMu protects output, outputBytes, viewCount, and lastViewedOutputBytes.
	outputMu              sync.RWMutex
	output                strings.Builder
	outputBytes           int
	viewCount             int
	lastViewedOutputBytes int
}

func (t *task) loadStatus() taskStatus {
	return taskStatus(t.status.Load())
}

func (t *task) storeStatus(s taskStatus) {
	t.status.Store(int32(s))
}

func (t *task) casStatus(old, next taskStatus) bool {
	return t.status.CompareAndSwap(int32(old), int32(next))
}

// writeOutput appends content to the task's live output buffer, respecting the
// maxOutputBytes cap. It is safe for concurrent use.
func (t *task) writeOutput(content string) {
	t.outputMu.Lock()
	defer t.outputMu.Unlock()

	if t.outputBytes < maxOutputBytes {
		n, _ := t.output.WriteString(content)
		t.outputBytes += n
	}
}

// formatView builds the human-readable output section for HandleView.
// It covers all terminal and in-progress states. The caller supplies the
// pre-loaded status and elapsed duration.
func (t *task) formatView(status taskStatus, elapsed time.Duration) string {
	var out strings.Builder
	fmt.Fprintf(&out, "Task ID: %s\n", t.id)
	fmt.Fprintf(&out, "Agent:   %s\n", t.agentName)
	fmt.Fprintf(&out, "Status:  %s\n", status)
	fmt.Fprintf(&out, "Runtime: %s\n", elapsed)
	out.WriteString("\n--- Output ---\n")

	switch status {
	case taskCompleted:
		if t.result != "" {
			out.WriteString(t.result)
		} else {
			out.WriteString("<no output>")
		}

	case taskFailed:
		out.WriteString("<task failed>")
		if t.errMsg != "" {
			fmt.Fprintf(&out, "\nError: %s", t.errMsg)
		}

	case taskStopped:
		out.WriteString("<task was stopped>")

	default: // taskRunning (or any unexpected value)
		t.outputMu.Lock()
		progress := t.output.String()
		truncated := t.outputBytes >= maxOutputBytes
		currentBytes := t.outputBytes

		if currentBytes == t.lastViewedOutputBytes {
			t.viewCount++
		} else {
			t.viewCount = 1
			t.lastViewedOutputBytes = currentBytes
		}
		viewCount := t.viewCount
		t.outputMu.Unlock()

		if progress != "" {
			out.WriteString(progress)
			if truncated {
				out.WriteString("\n\n[output truncated at 10MB limit — still running...]")
			} else {
				out.WriteString("\n\n[still running...]")
			}
		} else {
			out.WriteString("<no output yet — still running>")
		}
		if viewCount > 1 {
			fmt.Fprintf(&out, "\n\n[No new output since last check — poll #%d]", viewCount)
		}
	}

	return out.String()
}

// Handler owns all background agent tasks and provides tool handlers.
type Handler struct {
	runner Runner
	wg     sync.WaitGroup
	tasks  *concurrent.Map[string, *task]
}

// NewHandler creates a new Handler with the given Runner.
func NewHandler(runner Runner) *Handler {
	return &Handler{
		runner: runner,
		tasks:  concurrent.NewMap[string, *task](),
	}
}

func newTaskID() string {
	return "agent_task_" + uuid.New().String()
}

func (h *Handler) runningTaskCount() int {
	var count int
	h.tasks.Range(func(_ string, t *task) bool {
		if t.loadStatus() == taskRunning {
			count++
		}
		return true
	})
	return count
}

func (h *Handler) totalTaskCount() int {
	return h.tasks.Length()
}

func (h *Handler) pruneCompleted() {
	var toDelete []string
	h.tasks.Range(func(id string, t *task) bool {
		if s := t.loadStatus(); s != taskRunning {
			toDelete = append(toDelete, id)
		}
		return true
	})
	for _, id := range toDelete {
		h.tasks.Delete(id)
	}
}

// HandleRun starts a sub-agent task asynchronously and returns a task ID immediately.
func (h *Handler) HandleRun(ctx context.Context, sess *session.Session, toolCall tools.ToolCall) (*tools.ToolCallResult, error) {
	var params RunBackgroundAgentArgs
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	if strings.TrimSpace(params.Agent) == "" {
		return tools.ResultError("agent name must not be empty"), nil
	}
	if strings.TrimSpace(params.Task) == "" {
		return tools.ResultError("task must not be empty"), nil
	}

	subAgentNames := h.runner.CurrentAgentSubAgentNames()
	if !slices.Contains(subAgentNames, params.Agent) {
		if len(subAgentNames) > 0 {
			return tools.ResultError(fmt.Sprintf("agent %q is not in the sub-agents list. Available: %s", params.Agent, strings.Join(subAgentNames, ", "))), nil
		}
		return tools.ResultError(fmt.Sprintf("agent %q is not in the sub-agents list. This agent has no sub-agents configured.", params.Agent)), nil
	}

	// Enforce concurrency cap.
	if h.runningTaskCount() >= maxConcurrentTasks {
		return tools.ResultError(fmt.Sprintf("maximum concurrent background agent tasks (%d) reached; stop or wait for existing tasks to complete", maxConcurrentTasks)), nil
	}

	// Enforce total cap, pruning finished tasks first.
	if h.totalTaskCount() >= maxTotalTasks {
		h.pruneCompleted()
		if h.totalTaskCount() >= maxTotalTasks {
			return tools.ResultError(fmt.Sprintf("maximum total background agent tasks (%d) reached; view and discard old tasks first", maxTotalTasks)), nil
		}
	}

	taskID := newTaskID()

	// Use WithoutCancel so the background task is not killed when the
	// parent message context is cancelled (e.g. the user sends a new
	// message in the TUI). The task can still be explicitly stopped
	// via HandleStop which calls cancel().
	taskCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))

	t := &task{
		id:        taskID,
		agentName: params.Agent,
		taskDesc:  params.Task,
		cancel:    cancel,
		startTime: time.Now(),
	}
	t.storeStatus(taskRunning)
	h.tasks.Store(taskID, t)

	h.wg.Go(func() {
		defer cancel()

		slog.DebugContext(ctx, "Starting background agent task", "task_id", taskID, "agent", params.Agent)

		result := h.runner.RunAgent(taskCtx, RunParams{
			AgentName:      params.Agent,
			Task:           params.Task,
			ExpectedOutput: params.ExpectedOutput,
			ParentSession:  sess,
			OnContent:      t.writeOutput,
		})

		if result.ErrMsg != "" {
			t.errMsg = result.ErrMsg
			t.storeStatus(taskFailed)
			slog.DebugContext(ctx, "Background agent task failed", "task_id", taskID, "agent", params.Agent, "error", result.ErrMsg)
			return
		}

		if taskCtx.Err() != nil && t.loadStatus() == taskRunning {
			t.storeStatus(taskStopped)
			slog.DebugContext(ctx, "Background agent task stopped", "task_id", taskID)
			return
		}

		// Write result before CAS so readers who observe taskCompleted
		// always see the populated result field.
		t.result = result.Result
		if t.casStatus(taskRunning, taskCompleted) {
			slog.DebugContext(ctx, "Background agent task completed", "task_id", taskID, "agent", params.Agent)
		}
	})

	return tools.ResultSuccess(fmt.Sprintf("Background agent task started with ID: %s\nAgent: %s\nTask: %s",
		taskID, params.Agent, params.Task)), nil
}

// HandleList lists all background agent tasks.
func (h *Handler) HandleList(_ context.Context, _ *session.Session, _ tools.ToolCall) (*tools.ToolCallResult, error) {
	var out strings.Builder
	out.WriteString("Background Agent Tasks:\n\n")

	var count int
	h.tasks.Range(func(_ string, t *task) bool {
		count++
		elapsed := time.Since(t.startTime).Round(time.Second)
		fmt.Fprintf(&out, "ID: %s\n", t.id)
		fmt.Fprintf(&out, "  Agent:   %s\n", t.agentName)
		fmt.Fprintf(&out, "  Status:  %s\n", t.loadStatus())
		fmt.Fprintf(&out, "  Runtime: %s\n", elapsed)
		out.WriteString("\n")
		return true
	})

	if count == 0 {
		out.WriteString("No background agent tasks found.\n")
	}

	return tools.ResultSuccess(out.String()), nil
}

// HandleView returns the output and status of a specific background agent task.
func (h *Handler) HandleView(_ context.Context, _ *session.Session, toolCall tools.ToolCall) (*tools.ToolCallResult, error) {
	var params ViewBackgroundAgentArgs
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	t, exists := h.tasks.Load(params.TaskID)
	if !exists {
		return tools.ResultError("task not found: " + params.TaskID), nil
	}

	status := t.loadStatus()
	elapsed := time.Since(t.startTime).Round(time.Second)

	return tools.ResultSuccess(t.formatView(status, elapsed)), nil
}

// HandleStop cancels a running background agent task.
func (h *Handler) HandleStop(_ context.Context, _ *session.Session, toolCall tools.ToolCall) (*tools.ToolCallResult, error) {
	var params StopBackgroundAgentArgs
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	t, exists := h.tasks.Load(params.TaskID)
	if !exists {
		return tools.ResultError("task not found: " + params.TaskID), nil
	}

	if !t.casStatus(taskRunning, taskStopped) {
		return tools.ResultError(fmt.Sprintf("task %s is not running (status: %s)", params.TaskID, t.loadStatus())), nil
	}

	t.cancel()

	return tools.ResultSuccess(fmt.Sprintf("Background agent task %s stopped.", params.TaskID)), nil
}

// StopAll cancels all running tasks and waits for their goroutines to exit.
// Called during runtime shutdown to ensure clean teardown.
func (h *Handler) StopAll() {
	h.tasks.Range(func(_ string, t *task) bool {
		if t.casStatus(taskRunning, taskStopped) {
			t.cancel()
		}
		return true
	})
	h.wg.Wait()
}

// RegisterHandlers adds all background agent tool handlers to the given
// dispatch map, keyed by tool name.
func (h *Handler) RegisterHandlers(register func(name string, fn func(context.Context, *session.Session, tools.ToolCall) (*tools.ToolCallResult, error))) {
	register(ToolNameRunBackgroundAgent, h.HandleRun)
	register(ToolNameListBackgroundAgents, h.HandleList)
	register(ToolNameViewBackgroundAgent, h.HandleView)
	register(ToolNameStopBackgroundAgent, h.HandleStop)
}

// NewToolSet returns a lightweight ToolSet for registering background agent
// tool definitions and instructions. It does not require a Runner and is
// suitable for use in the teamloader registry.
func NewToolSet() tools.ToolSet {
	return &toolSet{}
}

// toolSet provides tool definitions and instructions without a Runner.
type toolSet struct{}

func (t *toolSet) Tools(context.Context) ([]tools.Tool, error) {
	return backgroundAgentTools(), nil
}

func (t *toolSet) Instructions() string {
	return `# Background Agent Tasks

Use background agent tasks to dispatch work to sub-agents concurrently.

- **run_background_agent**: Start a command, returns task ID. The sub-agent runs with all tools pre-approved — use only with trusted sub-agents and well-scoped tasks.
- **list_background_agents**: Show all tasks with status and runtime
- **view_background_agent**: Get output and status of a task by task_id
- **stop_background_agent**: Terminate a task by task_id

**Notes**: Output capped at 10MB per task. All tasks auto-terminate when the agent stops.`
}

func backgroundAgentTools() []tools.Tool {
	return []tools.Tool{
		{
			Name:     ToolNameRunBackgroundAgent,
			Category: "transfer",
			Description: `Start a sub-agent task in the background and return immediately with a task ID.
Use this to dispatch work to multiple sub-agents concurrently. The sub-agent runs with all tools
pre-approved — use only with trusted sub-agents and well-scoped tasks. Check progress with
view_background_agent and collect results once the task is complete.`,
			Parameters:  tools.MustSchemaFor[RunBackgroundAgentArgs](),
			Annotations: tools.ToolAnnotations{Title: "Run Background Agent"},
		},
		{
			Name:        ToolNameListBackgroundAgents,
			Category:    "transfer",
			Description: `List all background agent tasks with their status and runtime.`,
			Annotations: tools.ToolAnnotations{
				Title:        "List Background Agents",
				ReadOnlyHint: true,
			},
		},
		{
			Name:        ToolNameViewBackgroundAgent,
			Category:    "transfer",
			Description: `View the output and status of a specific background agent task by task ID. Returns live buffered output if still running, or the final result if complete.`,
			Parameters:  tools.MustSchemaFor[ViewBackgroundAgentArgs](),
			Annotations: tools.ToolAnnotations{
				Title:        "View Background Agent",
				ReadOnlyHint: true,
			},
		},
		{
			Name:        ToolNameStopBackgroundAgent,
			Category:    "transfer",
			Description: `Stop a running background agent task by task ID.`,
			Parameters:  tools.MustSchemaFor[StopBackgroundAgentArgs](),
			Annotations: tools.ToolAnnotations{
				Title: "Stop Background Agent",
			},
		},
	}
}
