package mcp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	tasksExtension          = "io.modelcontextprotocol/tasks"
	tasksMinProtocolVersion = "2026-07-28"
	internalTaskTokenMeta   = "docker-agent/createdTaskToken"
	defaultTaskPollInterval = time.Second
	taskCancelTimeout       = 5 * time.Second
)

type taskStatus string

const (
	taskStatusWorking       taskStatus = "working"
	taskStatusInputRequired taskStatus = "input_required"
	taskStatusCompleted     taskStatus = "completed"
	taskStatusFailed        taskStatus = "failed"
	taskStatusCancelled     taskStatus = "cancelled"
)

type taskData struct {
	TaskID         string                `json:"taskId"`
	Status         taskStatus            `json:"status"`
	StatusMessage  string                `json:"statusMessage,omitempty"`
	CreatedAt      string                `json:"createdAt"`
	LastUpdatedAt  string                `json:"lastUpdatedAt"`
	TTLMs          *int64                `json:"ttlMs"`
	PollIntervalMs *int64                `json:"pollIntervalMs,omitempty"`
	InputRequests  gomcp.InputRequestMap `json:"inputRequests,omitempty"`
	Result         json.RawMessage       `json:"result,omitempty"`
	Error          *jsonrpc.Error        `json:"error,omitempty"`
}

type taskGetParams struct {
	gomcp.ParamsBase

	TaskID string `json:"taskId"`
}

type taskGetResult struct {
	gomcp.ResultBase
	taskData

	ResultType string `json:"resultType"`
}

type taskUpdateParams struct {
	gomcp.ParamsBase

	TaskID         string                 `json:"taskId"`
	InputResponses gomcp.InputResponseMap `json:"inputResponses"`
}

type taskUpdateResult struct {
	gomcp.ResultBase

	ResultType string `json:"resultType"`
}

type taskCancelParams struct {
	gomcp.ParamsBase

	TaskID string `json:"taskId"`
}

type taskCancelResult struct {
	gomcp.ResultBase

	ResultType string `json:"resultType"`
}

// taskExecutionError marks an error that occurred after a server created an
// asynchronous task. Callers use isTaskExecutionError to avoid blindly
// re-issuing a non-idempotent tools/call on reconnect.
type taskExecutionError struct {
	err error
}

func (e *taskExecutionError) Error() string { return e.err.Error() }
func (e *taskExecutionError) Unwrap() error { return e.err }

func isTaskExecutionError(err error) bool {
	var target *taskExecutionError
	return errors.As(err, &target)
}

// Experimental SEP-2663 Tasks client (spec 2026-07-28).
// The SDK decodes tools/call responses into CallToolResult, which drops
// CreateTaskResult fields. Transport adapters inspect raw JSON-RPC responses
// for resultType "task", store the task handle here under an unguessable
// per-client token, and pass that token through _meta so CallTool can
// transparently drive tasks/get, tasks/update, and tasks/cancel.
type taskClient struct {
	mu                  sync.Mutex
	created             map[string]*taskData
	tokenPrefix         string
	nextID              atomic.Uint64
	sanitizeError       func(error) error
	defaultPollInterval time.Duration
}

func newTaskClient(client *gomcp.Client) (*taskClient, error) {
	if err := gomcp.AddSendingCustomMethod[*taskGetParams, *taskGetResult](client, "tasks/get"); err != nil {
		return nil, err
	}
	if err := gomcp.AddSendingCustomMethod[*taskUpdateParams, *taskUpdateResult](client, "tasks/update"); err != nil {
		return nil, err
	}
	if err := gomcp.AddSendingCustomMethod[*taskCancelParams, *taskCancelResult](client, "tasks/cancel"); err != nil {
		return nil, err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, fmt.Errorf("generating task token prefix: %w", err)
	}
	return &taskClient{
		created:     make(map[string]*taskData),
		tokenPrefix: hex.EncodeToString(nonce[:]),
	}, nil
}

func (c *taskClient) safeError(err error) error {
	if err != nil && c.sanitizeError != nil {
		return c.sanitizeError(err)
	}
	return err
}

func (c *taskClient) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.created)
}

func (c *taskClient) storeCreatedTask(task *taskData) string {
	token := c.tokenPrefix + "-" + strconv.FormatUint(c.nextID.Add(1), 10)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.created[token] = task
	return token
}

func (c *taskClient) ownsToken(token string) bool {
	return c != nil && c.tokenPrefix != "" && strings.HasPrefix(token, c.tokenPrefix+"-")
}

func (c *taskClient) takeCreatedTask(token string) (*taskData, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	task, ok := c.created[token]
	if ok {
		delete(c.created, token)
	}
	return task, ok
}

func (c *taskClient) pollDelay(task *taskData) time.Duration {
	if task != nil && task.PollIntervalMs != nil && *task.PollIntervalMs > 0 {
		return time.Duration(*task.PollIntervalMs) * time.Millisecond
	}
	if c.defaultPollInterval > 0 {
		return c.defaultPollInterval
	}
	return defaultTaskPollInterval
}

func taskDeadline(task *taskData) (time.Time, bool) {
	if task == nil || task.TTLMs == nil || *task.TTLMs <= 0 || task.CreatedAt == "" {
		return time.Time{}, false
	}
	createdAt, err := time.Parse(time.RFC3339Nano, task.CreatedAt)
	if err != nil {
		return time.Time{}, false
	}
	return createdAt.Add(time.Duration(*task.TTLMs) * time.Millisecond), true
}

func withTaskDeadline(ctx context.Context, task *taskData) (context.Context, context.CancelFunc, bool) {
	deadline, ok := taskDeadline(task)
	if !ok {
		return ctx, func() {}, false
	}
	if callerDeadline, hasCaller := ctx.Deadline(); hasCaller && !deadline.Before(callerDeadline) {
		return ctx, func() {}, false
	}
	ttlCtx, cancel := context.WithDeadline(ctx, deadline)
	return ttlCtx, cancel, true
}

func tasksSupported(initResult *gomcp.InitializeResult) bool {
	if initResult == nil || initResult.ProtocolVersion < tasksMinProtocolVersion {
		return false
	}
	if initResult.Capabilities == nil {
		return false
	}
	_, ok := initResult.Capabilities.Extensions[tasksExtension]
	return ok
}

// inspectResponse checks whether resp carries a SEP-2663 CreateTaskResult
// (resultType == "task"). If so, it validates that the originating request was
// tools/call, stores the parsed task under a private token, and rewrites
// resp.Result to carry only that token in _meta.
func (c *taskClient) inspectResponse(method string, resp *jsonrpc.Response) (*jsonrpc.Response, bool, error) {
	if resp == nil || resp.Error != nil || len(resp.Result) == 0 {
		return resp, false, nil
	}
	var envelope struct {
		ResultType string `json:"resultType"`
	}
	if err := json.Unmarshal(resp.Result, &envelope); err != nil || envelope.ResultType != "task" {
		return resp, false, nil
	}
	if method != "tools/call" {
		return nil, false, fmt.Errorf("invalid MCP response: unexpected CreateTaskResult for method %q", method)
	}
	var task taskData
	if err := json.Unmarshal(resp.Result, &task); err != nil {
		return nil, false, fmt.Errorf("decoding CreateTaskResult: %w", err)
	}
	if task.TaskID == "" {
		return nil, false, errors.New("invalid MCP CreateTaskResult: missing taskId")
	}
	token := c.storeCreatedTask(&task)
	rewritten, err := json.Marshal(struct {
		Meta map[string]any `json:"_meta"`
	}{
		Meta: map[string]any{internalTaskTokenMeta: token},
	})
	if err != nil {
		c.takeCreatedTask(token)
		return nil, false, err
	}
	clone := *resp
	clone.Result = rewritten
	return &clone, true, nil
}

func (c *taskClient) cancelBestEffort(ctx context.Context, s *gomcp.ClientSession, taskID string) {
	cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), taskCancelTimeout)
	defer cancel()
	if _, err := gomcp.CallCustomMethod[*taskCancelParams, *taskCancelResult](cancelCtx, s, "tasks/cancel", &taskCancelParams{TaskID: taskID}); err != nil {
		slog.DebugContext(cancelCtx, "Best-effort MCP tasks/cancel failed", "task_id", taskID, "error", c.safeError(err))
	}
}

func (c *taskClient) waitForTask(
	ctx context.Context,
	s *gomcp.ClientSession,
	initial *taskData,
	elicit func(context.Context, *gomcp.ElicitParams) (*gomcp.ElicitResult, error),
) (*gomcp.CallToolResult, error) {
	if !tasksSupported(s.InitializeResult()) {
		return nil, &taskExecutionError{err: errors.New("MCP server returned a task result without negotiating io.modelcontextprotocol/tasks")}
	}

	current := initial
	fromPoll := false
	answered := make(map[string]bool)

	for {
		skipSleep := false

		switch current.Status {
		case taskStatusCompleted:
			trimmed := bytes.TrimSpace(current.Result)
			if len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
				var result gomcp.CallToolResult
				if err := json.Unmarshal(current.Result, &result); err != nil {
					return nil, &taskExecutionError{err: fmt.Errorf("decoding completed MCP task %q result: %w", current.TaskID, err)}
				}
				return &result, nil
			}
			if fromPoll {
				return nil, &taskExecutionError{err: fmt.Errorf("MCP task %q completed without a result", current.TaskID)}
			}
			// CreateTaskResult carries Task metadata without DetailedTask payload;
			// poll tasks/get immediately to retrieve the final result.
			skipSleep = true

		case taskStatusFailed:
			if current.Error != nil {
				return nil, &taskExecutionError{err: c.safeError(current.Error)}
			}
			if fromPoll {
				if current.StatusMessage != "" {
					return nil, &taskExecutionError{err: fmt.Errorf("MCP task %q failed: %s", current.TaskID, current.StatusMessage)}
				}
				return nil, &taskExecutionError{err: fmt.Errorf("MCP task %q failed", current.TaskID)}
			}
			skipSleep = true

		case taskStatusCancelled:
			if current.Error != nil {
				return nil, &taskExecutionError{err: c.safeError(current.Error)}
			}
			if fromPoll {
				if current.StatusMessage != "" {
					return nil, &taskExecutionError{err: fmt.Errorf("MCP task %q was cancelled: %s", current.TaskID, current.StatusMessage)}
				}
				return nil, &taskExecutionError{err: fmt.Errorf("MCP task %q was cancelled", current.TaskID)}
			}
			skipSleep = true

		case taskStatusInputRequired:
			if !fromPoll && len(current.InputRequests) == 0 {
				skipSleep = true
			} else if len(current.InputRequests) > 0 {
				var pendingKeys []string
				for key := range current.InputRequests {
					if !answered[key] {
						pendingKeys = append(pendingKeys, key)
					}
				}
				if len(pendingKeys) > 0 {
					slices.Sort(pendingKeys)
					responses := make(gomcp.InputResponseMap, len(pendingKeys))
					opCtx, opCancel, hasTTL := withTaskDeadline(ctx, current)
					for _, key := range pendingKeys {
						ir := current.InputRequests[key]
						switch p := ir.(type) {
						case *gomcp.ElicitParams:
							resp, err := elicit(opCtx, p)
							if err != nil {
								expired := hasTTL && opCtx.Err() != nil
								opCancel()
								if ctx.Err() != nil {
									c.cancelBestEffort(ctx, s, current.TaskID)
									return nil, ctx.Err()
								}
								if expired {
									return nil, &taskExecutionError{err: fmt.Errorf("MCP task %q expired after TTL", current.TaskID)}
								}
								c.cancelBestEffort(ctx, s, current.TaskID)
								return nil, &taskExecutionError{err: fmt.Errorf("fulfilling input request %q for MCP task %q: %w", key, current.TaskID, err)}
							}
							responses[key] = resp
						default:
							opCancel()
							c.cancelBestEffort(ctx, s, current.TaskID)
							return nil, &taskExecutionError{err: fmt.Errorf("unsupported input request type %T for MCP task %q", ir, current.TaskID)}
						}
					}
					_, updateErr := gomcp.CallCustomMethod[*taskUpdateParams, *taskUpdateResult](opCtx, s, "tasks/update", &taskUpdateParams{
						TaskID:         current.TaskID,
						InputResponses: responses,
					})
					updateExpired := hasTTL && opCtx.Err() != nil
					opCancel()
					if updateErr != nil {
						if ctx.Err() != nil {
							c.cancelBestEffort(ctx, s, current.TaskID)
							return nil, ctx.Err()
						}
						if updateExpired {
							return nil, &taskExecutionError{err: fmt.Errorf("MCP task %q expired after TTL", current.TaskID)}
						}
						return nil, &taskExecutionError{err: c.safeError(updateErr)}
					}
					for _, key := range pendingKeys {
						answered[key] = true
					}
				}
			}

		case taskStatusWorking:
			// Continue polling.

		default:
			return nil, &taskExecutionError{err: fmt.Errorf("MCP task %q returned unknown status %q", current.TaskID, current.Status)}
		}

		if deadline, ok := taskDeadline(current); ok && !time.Now().Before(deadline) {
			return nil, &taskExecutionError{err: fmt.Errorf("MCP task %q expired after TTL", current.TaskID)}
		}

		if !skipSleep {
			delay := c.pollDelay(current)
			if deadline, ok := taskDeadline(current); ok {
				remaining := time.Until(deadline)
				if remaining <= 0 {
					return nil, &taskExecutionError{err: fmt.Errorf("MCP task %q expired after TTL", current.TaskID)}
				}
				if remaining < delay {
					delay = remaining
				}
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				c.cancelBestEffort(ctx, s, current.TaskID)
				return nil, ctx.Err()
			case <-timer.C:
			}
		}

		if err := ctx.Err(); err != nil {
			c.cancelBestEffort(ctx, s, current.TaskID)
			return nil, err
		}
		if deadline, ok := taskDeadline(current); ok && !time.Now().Before(deadline) {
			return nil, &taskExecutionError{err: fmt.Errorf("MCP task %q expired after TTL", current.TaskID)}
		}

		pollCtx, pollCancel, hasTTL := withTaskDeadline(ctx, current)
		next, err := gomcp.CallCustomMethod[*taskGetParams, *taskGetResult](pollCtx, s, "tasks/get", &taskGetParams{
			TaskID: current.TaskID,
		})
		pollExpired := hasTTL && pollCtx.Err() != nil
		pollCancel()
		if err != nil {
			if ctx.Err() != nil {
				c.cancelBestEffort(ctx, s, current.TaskID)
				return nil, ctx.Err()
			}
			if pollExpired {
				return nil, &taskExecutionError{err: fmt.Errorf("MCP task %q expired after TTL", current.TaskID)}
			}
			return nil, &taskExecutionError{err: c.safeError(err)}
		}
		current = &next.taskData
		if current.TaskID == "" {
			current.TaskID = initial.TaskID
		}
		fromPoll = true
	}
}

type taskTransport struct {
	gomcp.Transport

	tasks *taskClient
}

func (t *taskTransport) Connect(ctx context.Context) (gomcp.Connection, error) {
	conn, err := t.Transport.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &taskConnection{
		Connection:     conn,
		tasks:          t.tasks,
		pendingMethods: make(map[string]string),
	}, nil
}

type taskConnection struct {
	gomcp.Connection

	tasks          *taskClient
	mu             sync.Mutex
	pendingMethods map[string]string
}

func formatRawJSONID(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", false
	}
	var id any
	if err := json.Unmarshal(raw, &id); err != nil || id == nil {
		return "", false
	}
	return formatSubscriptionID(id), true
}

func (c *taskConnection) deletePendingMethod(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pendingMethods, key)
}

func (c *taskConnection) Write(ctx context.Context, msg jsonrpc.Message) error {
	var trackedKey string
	if req, ok := msg.(*jsonrpc.Request); ok {
		if req.IsCall() {
			trackedKey = formatSubscriptionID(req.ID.Raw())
			c.mu.Lock()
			c.pendingMethods[trackedKey] = req.Method
			c.mu.Unlock()
		} else if req.Method == "notifications/cancelled" && len(req.Params) > 0 {
			var p struct {
				RequestID json.RawMessage `json:"requestId"`
			}
			if json.Unmarshal(req.Params, &p) == nil {
				if cancelKey, ok := formatRawJSONID(p.RequestID); ok {
					c.deletePendingMethod(cancelKey)
				}
			}
		}
	}
	err := c.Connection.Write(ctx, msg)
	if err != nil && trackedKey != "" {
		c.deletePendingMethod(trackedKey)
	}
	return err
}

func (c *taskConnection) Read(ctx context.Context) (jsonrpc.Message, error) {
	msg, err := c.Connection.Read(ctx)
	if err != nil {
		return nil, err
	}
	resp, ok := msg.(*jsonrpc.Response)
	if !ok {
		return msg, nil
	}
	key := formatSubscriptionID(resp.ID.Raw())
	c.mu.Lock()
	method, found := c.pendingMethods[key]
	delete(c.pendingMethods, key)
	c.mu.Unlock()
	if !found {
		return resp, nil
	}

	rewritten, _, err := c.tasks.inspectResponse(method, resp)
	if err != nil {
		return nil, err
	}
	return rewritten, nil
}

func (c *taskConnection) Close() error {
	c.tasks.close()
	c.mu.Lock()
	clear(c.pendingMethods)
	c.mu.Unlock()
	return c.Connection.Close()
}

// SetExperimentalTasks configures SEP-2663 Tasks support before the toolset starts.
func (ts *Toolset) SetExperimentalTasks(enabled bool) {
	if client, ok := ts.mcpClient.(interface{ setExperimentalTasks(enabled bool) }); ok {
		client.setExperimentalTasks(enabled)
	}
}

func (c *sessionClient) setExperimentalTasks(enabled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.experimentalTasks = enabled
}

func (c *sessionClient) tasksEnabled() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.experimentalTasks
}

func (c *sessionClient) closeTasks() {
	c.mu.RLock()
	tasks := c.tasks
	c.mu.RUnlock()
	if tasks != nil {
		tasks.close()
	}
}
