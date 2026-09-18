package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

const (
	defaultWaitTimeout = 300
	maxWaitTimeout     = 3600
	maxWaitOutputBytes = 4096
)

// WaitBackgroundAgentsArgs specifies the tasks to join and the shared wait budget.
type WaitBackgroundAgentsArgs struct {
	TaskIDs []string `json:"task_ids" jsonschema:"IDs returned by run_background_agent. Provide 1 to 100 distinct task IDs."`
	Timeout int      `json:"timeout,omitempty" jsonschema:"Maximum seconds to wait for the whole group (default 300; 0 uses the default; maximum 3600). Unfinished tasks keep running after a timeout."`
}

type waitResult struct {
	AllDone  bool             `json:"all_done"`
	TimedOut bool             `json:"timed_out,omitempty"`
	Error    string           `json:"error,omitempty"`
	Tasks    []waitTaskResult `json:"tasks"`
}

type waitTaskResult struct {
	TaskID    string `json:"task_id"`
	Status    string `json:"status"`
	Done      bool   `json:"done"`
	Output    string `json:"output,omitempty"`
	Error     string `json:"error,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// HandleWait joins a fixed set of tasks without cancelling their execution.
func (h *Handler) HandleWait(ctx context.Context, _ *session.Session, toolCall tools.ToolCall) (*tools.ToolCallResult, error) {
	var params WaitBackgroundAgentsArgs
	if err := tools.UnmarshalToolArguments(ctx, toolCall, &params); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if len(params.TaskIDs) == 0 || len(params.TaskIDs) > maxTotalTasks {
		return tools.ResultError(fmt.Sprintf("task_ids must contain 1 to %d distinct task IDs", maxTotalTasks)), nil
	}
	if params.Timeout < 0 || params.Timeout > maxWaitTimeout {
		return tools.ResultError(fmt.Sprintf("timeout must be between 0 and %d seconds", maxWaitTimeout)), nil
	}
	seen := make(map[string]bool, len(params.TaskIDs))
	for _, id := range params.TaskIDs {
		if strings.TrimSpace(id) == "" {
			return tools.ResultError("task IDs must not be empty"), nil
		}
		if seen[id] {
			return tools.ResultError("duplicate task ID: " + id), nil
		}
		seen[id] = true
	}

	// Retain references so pruning cannot lose a task while the join is waiting.
	tasks := make([]*task, len(params.TaskIDs))
	for i, id := range params.TaskIDs {
		tasks[i], _ = h.tasks.Load(id)
	}
	timeout := params.Timeout
	if timeout == 0 {
		timeout = defaultWaitTimeout
	}
	timer := time.NewTimer(time.Duration(timeout) * time.Second)
	defer timer.Stop()

	result := waitResult{AllDone: true, Tasks: make([]waitTaskResult, 0, len(tasks))}
wait:
	for _, t := range tasks {
		if t == nil {
			continue
		}
		// Prefer an already-finished task even if the wait budget has expired.
		select {
		case <-t.done:
			continue
		default:
		}
		select {
		case <-t.done:
		case <-timer.C:
			result.TimedOut = true
			break wait
		case <-ctx.Done():
			result.Error = "Wait cancelled: " + ctx.Err().Error()
			break wait
		}
	}

	isError := result.Error != ""
	for i, t := range tasks {
		entry := waitTaskResult{TaskID: params.TaskIDs[i], Status: "not_found", Error: "Task not found; it may have been pruned."}
		if t != nil {
			entry = t.waitResult()
		} else {
			isError = true
		}
		result.AllDone = result.AllDone && entry.Done
		result.Tasks = append(result.Tasks, entry)
	}
	out := tools.ResultJSON(result)
	out.IsError = isError
	return out, nil
}

func (t *task) waitResult() waitTaskResult {
	entry := waitTaskResult{TaskID: t.id}
	select {
	case <-t.done:
		entry.Done = true
	default:
	}
	status := t.loadStatus()
	entry.Status = status.String()
	switch status {
	case taskCompleted:
		entry.Output = chat.TruncateUTF8Bytes(t.result, maxWaitOutputBytes)
		entry.Truncated = len(entry.Output) < len(t.result)
	case taskFailed:
		entry.Error = chat.TruncateUTF8Bytes(t.errMsg, maxWaitOutputBytes)
		entry.Truncated = len(entry.Error) < len(t.errMsg)
	default:
		t.outputMu.RLock()
		defer t.outputMu.RUnlock()
		entry.Output = chat.TruncateUTF8Bytes(t.output.String(), maxWaitOutputBytes)
		entry.Truncated = len(entry.Output) < t.outputBytes || t.outputBytes >= maxOutputBytes
	}
	return entry
}
