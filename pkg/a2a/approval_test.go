package a2a

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genai"

	dagent "github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/servesafety"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestInputRequiredEvent(t *testing.T) {
	t.Parallel()

	ev := inputRequiredEvent("root", tools.ToolCall{
		ID:       "call-1",
		Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"ls"}`},
	})

	assert.Equal(t, "root", ev.Author)
	assert.Equal(t, []string{"call-1"}, ev.LongRunningToolIDs)
	require.NotNil(t, ev.Content)
	require.Len(t, ev.Content.Parts, 1)
	fc := ev.Content.Parts[0].FunctionCall
	require.NotNil(t, fc)
	assert.Equal(t, "call-1", fc.ID)
	assert.Equal(t, "shell", fc.Name)
	assert.Equal(t, "ls", fc.Args["cmd"])
}

func TestInputRequiredEvent_MalformedArgsStillSurfacesCall(t *testing.T) {
	t.Parallel()

	ev := inputRequiredEvent("root", tools.ToolCall{
		ID:       "call-2",
		Function: tools.FunctionCall{Name: "shell", Arguments: "not json"},
	})
	fc := ev.Content.Parts[0].FunctionCall
	require.NotNil(t, fc)
	assert.Equal(t, "call-2", fc.ID)
	assert.Nil(t, fc.Args)
	assert.Equal(t, []string{"call-2"}, ev.LongRunningToolIDs)
}

func TestResumeDecision(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		response map[string]any
		wantType runtime.ResumeType
		wantTool string
		wantMsg  string
	}{
		{name: "approve", response: map[string]any{"decision": "approve"}, wantType: runtime.ResumeTypeApprove},
		{name: "approve-tool", response: map[string]any{"decision": "approve-tool"}, wantType: runtime.ResumeTypeApproveTool, wantTool: "shell"},
		{name: "reject with reason", response: map[string]any{"decision": "reject", "reason": "nope"}, wantType: runtime.ResumeTypeReject, wantMsg: "nope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := resumeDecision(&genai.FunctionResponse{ID: "call-1", Name: "shell", Response: tc.response}, "shell")
			assert.Equal(t, tc.wantType, req.Type)
			assert.Equal(t, tc.wantTool, req.ToolName)
			assert.Equal(t, tc.wantMsg, req.Reason)
		})
	}
}

func TestApprovalRegistry_BoundToTaskAndCall(t *testing.T) {
	t.Parallel()

	reg := newApprovalRegistry(t.Context(), true)
	events := make(chan runtime.Event)
	close(events)
	canceled := false
	reg.park(&pausedRun{taskID: "task-1", contextID: "ctx", callID: "call-1", events: events, cancel: func() { canceled = true }})

	_, ok := reg.take("task-2", "call-1")
	assert.False(t, ok, "another task must not take the run")
	_, ok = reg.take("task-1", "call-2")
	assert.False(t, ok, "a different call ID must not take the run")
	reg.cancel("task-2")
	assert.False(t, canceled, "cancelling another task must not cancel the run")

	run, ok := reg.take("task-1", "call-1")
	require.True(t, ok)
	assert.Equal(t, a2a.TaskID("task-1"), run.taskID)
}

// The run context keeps the invocation's values (trace context, baggage) but
// not its cancellation, and server shutdown still cancels it.
func TestApprovalRegistry_RunContextKeepsValuesAndServerLifetime(t *testing.T) {
	t.Parallel()

	type key struct{}
	server, stopServer := context.WithCancel(t.Context())
	reg := newApprovalRegistry(server, true)

	invocation, endInvocation := context.WithCancel(context.WithValue(t.Context(), key{}, "trace"))
	runCtx, cancel := reg.runContext(invocation)
	defer cancel()

	assert.Equal(t, "trace", runCtx.Value(key{}))
	endInvocation()
	require.NoError(t, runCtx.Err(), "a run must survive the invocation that parked it")
	stopServer()
	require.Eventually(t, func() bool { return runCtx.Err() != nil }, time.Second, 5*time.Millisecond)
}

func TestValidateReply(t *testing.T) {
	t.Parallel()

	reg := newApprovalRegistry(t.Context(), true)
	reg.park(&pausedRun{taskID: "task-1", contextID: "ctx-1", callID: "call-1", events: closedEvents(), cancel: func() {}})

	reply := func(taskID a2a.TaskID, contextID string, parts ...*a2a.Part) *a2a.Message {
		msg := a2a.NewMessage(a2a.MessageRoleUser, parts...)
		msg.TaskID, msg.ContextID = taskID, contextID
		return msg
	}
	response := func(id string, resp map[string]any) *a2a.Part {
		part := a2a.NewDataPart(map[string]any{"id": id, "name": "shell", "response": resp})
		part.SetMeta("adk_type", "function_response")
		return part
	}

	for _, tc := range []struct {
		name   string
		msg    *a2a.Message
		reason string
	}{
		{name: "approve", msg: reply("task-1", "ctx-1", response("call-1", map[string]any{"decision": "approve"}))},
		{name: "reject", msg: reply("task-1", "ctx-1", response("call-1", map[string]any{"decision": "reject"}))},
		{name: "other task untouched", msg: reply("task-2", "ctx-2", a2a.NewTextPart("hi"))},
		{name: "unknown decision", msg: reply("task-1", "ctx-1", response("call-1", map[string]any{"decision": "x"})), reason: "decision 'x' not allowed; expected [approve, approve-tool, reject]"},
		{name: "missing decision", msg: reply("task-1", "ctx-1", response("call-1", map[string]any{})), reason: "decision '' not allowed; expected [approve, approve-tool, reject]"},
		{name: "wrong call ID", msg: reply("task-1", "ctx-1", response("call-9", map[string]any{"decision": "approve"})), reason: "expected a function_response for call 'call-1'"},
		{name: "free text", msg: reply("task-1", "ctx-1", a2a.NewTextPart("yes")), reason: "expected a function_response for call 'call-1'"},
		{name: "new task in waiting context", msg: reply("", "ctx-1", a2a.NewTextPart("hi")), reason: "task task-1 is waiting for tool approval in this context; reply with its taskId or cancel it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := reg.validateReply(tc.msg)
			if tc.reason == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, a2a.ErrInvalidParams)
			var a2aErr *a2a.Error
			require.ErrorAs(t, err, &a2aErr)
			assert.Equal(t, tc.reason, a2aErr.Details["reason"])
		})
	}
}

func closedEvents() <-chan runtime.Event {
	ch := make(chan runtime.Event)
	close(ch)
	return ch
}

// approvalCanaryTool returns a tool that counts its executions.
func approvalCanaryTool(executions *atomic.Int32) tools.Tool {
	return tools.Tool{
		Name:        "approval_canary",
		Description: "test tool",
		Parameters:  map[string]any{},
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			executions.Add(1)
			return tools.ResultSuccess("done"), nil
		},
	}
}

func startToolApprovalServer(t *testing.T, tm *team.Team, store session.Store, toolApproval bool) *a2aclient.Client {
	t.Helper()
	client, _ := startApprovalServer(t, tm, store, RunOptions{ToolApproval: toolApproval})
	return client
}

// startApprovalServer serves tm and returns a client and the listen address.
func startApprovalServer(t *testing.T, tm *team.Team, store session.Store, opts RunOptions) (*a2aclient.Client, string) {
	t.Helper()

	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	// Strict always asks for confirmation, so the call reaches askUser (where
	// the tool-approval switch lives) rather than being auto-allowed or denied
	// by the mode itself.
	e, err := newServer(t.Context(), tm, "test.yaml", "root", store, newTestTaskDeps(t),
		servesafety.Resolved{Policy: session.SafetyPolicyStrict}, testWorkspaceRoot, ln.Addr().String(), opts)
	require.NoError(t, err)
	go func() { _ = e.Server.Serve(ln) }()
	t.Cleanup(func() { require.NoError(t, e.Server.Close()) })

	client, err := a2aclient.NewFromEndpoints(t.Context(), []*a2a.AgentInterface{
		a2a.NewAgentInterface(fmt.Sprintf("http://%s/invoke", ln.Addr().String()), a2a.TransportProtocolJSONRPC),
	})
	require.NoError(t, err)
	return client, ln.Addr().String()
}

func approvalTeam(executions *atomic.Int32) *team.Team {
	tool := approvalCanaryTool(executions)
	provider := &sequentialMockProvider{
		id:      modelsdev.NewID("test", "mock-model"),
		streams: []chat.MessageStream{toolCallStream(tool.Name), stopStream()},
	}
	root := dagent.New("root", "You are a test agent", dagent.WithModel(provider), dagent.WithTools(tool))
	return team.New(team.WithAgents(root))
}

// requireInputRequired asserts the result is a task parked in INPUT_REQUIRED
// and returns it along with the long-running function-call ID the client must
// answer.
func requireInputRequired(t *testing.T, res a2a.SendMessageResult) (*a2a.Task, string) {
	t.Helper()

	task, ok := res.(*a2a.Task)
	require.True(t, ok, "expected a task, got %T", res)
	require.Equal(t, a2a.TaskStateInputRequired, task.Status.State)
	require.NotNil(t, task.Status.Message)

	for _, part := range task.Status.Message.Parts {
		data, ok := part.Data().(map[string]any)
		if !ok {
			continue
		}
		if part.Meta()["adk_type"] != "function_call" {
			continue
		}
		assert.Equal(t, true, part.Meta()["adk_is_long_running"])
		id, _ := data["id"].(string)
		require.NotEmpty(t, id)
		return task, id
	}
	t.Fatalf("no function_call data part in status message: %+v", task.Status.Message.Parts)
	return nil, ""
}

// answerToolCall builds the resume message that replies to a parked call.
func answerToolCall(task *a2a.Task, callID string, response map[string]any) *a2a.Message {
	part := a2a.NewDataPart(map[string]any{
		"id":       callID,
		"name":     "approval_canary",
		"response": response,
	})
	part.SetMeta("adk_type", "function_response")
	msg := a2a.NewMessage(a2a.MessageRoleUser, part)
	msg.TaskID = task.ID
	msg.ContextID = task.ContextID
	return msg
}

func TestServer_ToolApproval_Approve(t *testing.T) {
	t.Parallel()

	var executions atomic.Int32
	client := startToolApprovalServer(t, approvalTeam(&executions), session.NewInMemorySessionStore(), true)

	res, err := client.SendMessage(t.Context(), &a2a.SendMessageRequest{
		Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("run the tool")),
	})
	require.NoError(t, err)
	task, callID := requireInputRequired(t, res)
	require.Zero(t, executions.Load(), "the tool must not run before approval")

	res2, err := client.SendMessage(t.Context(), &a2a.SendMessageRequest{
		Message: answerToolCall(task, callID, map[string]any{"decision": "approve"}),
	})
	require.NoError(t, err)
	done, ok := res2.(*a2a.Task)
	require.True(t, ok, "expected a task, got %T", res2)
	assert.Equal(t, a2a.TaskStateCompleted, done.Status.State)
	assert.Equal(t, int32(1), executions.Load(), "approval must run the tool")
}

func TestServer_ToolApproval_Reject(t *testing.T) {
	t.Parallel()

	var executions atomic.Int32
	client := startToolApprovalServer(t, approvalTeam(&executions), session.NewInMemorySessionStore(), true)

	res, err := client.SendMessage(t.Context(), &a2a.SendMessageRequest{
		Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("run the tool")),
	})
	require.NoError(t, err)
	task, callID := requireInputRequired(t, res)

	res2, err := client.SendMessage(t.Context(), &a2a.SendMessageRequest{
		Message: answerToolCall(task, callID, map[string]any{"decision": "reject", "reason": "no"}),
	})
	require.NoError(t, err)
	done, ok := res2.(*a2a.Task)
	require.True(t, ok, "expected a task, got %T", res2)
	assert.Equal(t, a2a.TaskStateCompleted, done.Status.State)
	assert.Zero(t, executions.Load(), "rejection must not run the tool")
}

func TestServer_ToolApproval_Cancel(t *testing.T) {
	t.Parallel()

	var executions atomic.Int32
	client := startToolApprovalServer(t, approvalTeam(&executions), session.NewInMemorySessionStore(), true)

	res, err := client.SendMessage(t.Context(), &a2a.SendMessageRequest{
		Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("run the tool")),
	})
	require.NoError(t, err)
	task, _ := requireInputRequired(t, res)

	canceled, err := client.CancelTask(t.Context(), &a2a.CancelTaskRequest{ID: task.ID})
	require.NoError(t, err)
	assert.Equal(t, a2a.TaskStateCanceled, canceled.Status.State)
	assert.Zero(t, executions.Load(), "a canceled task must not run the tool")
}

// Without the flag the non-interactive session denies the call rather than
// parking it in INPUT_REQUIRED, exactly as before the feature.
func TestServer_ToolApproval_DisabledDeniesInsteadOfParking(t *testing.T) {
	t.Parallel()

	var executions atomic.Int32
	client := startToolApprovalServer(t, approvalTeam(&executions), session.NewInMemorySessionStore(), false)

	res, err := client.SendMessage(t.Context(), &a2a.SendMessageRequest{
		Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("run the tool")),
	})
	require.NoError(t, err)
	if task, ok := res.(*a2a.Task); ok {
		assert.NotEqual(t, a2a.TaskStateInputRequired, task.Status.State, "no flag must never park in INPUT_REQUIRED")
	}
	assert.Zero(t, executions.Load(), "denied tool must not run")
}
