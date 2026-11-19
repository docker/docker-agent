package a2a

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"testing"

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

func functionResponseContent(response map[string]any) *genai.Content {
	return &genai.Content{Parts: []*genai.Part{{
		FunctionResponse: &genai.FunctionResponse{ID: "call-1", Name: "shell", Response: response},
	}}}
}

func TestResumeDecision(t *testing.T) {
	t.Parallel()

	run := &pausedRun{callID: "call-1", toolName: "shell"}

	for _, tc := range []struct {
		name     string
		content  *genai.Content
		wantType runtime.ResumeType
		wantTool string
		wantMsg  string
	}{
		{
			name:     "approve",
			content:  functionResponseContent(map[string]any{"decision": "approve"}),
			wantType: runtime.ResumeTypeApprove,
		},
		{
			name:     "approve-tool",
			content:  functionResponseContent(map[string]any{"decision": "approve-tool"}),
			wantType: runtime.ResumeTypeApproveTool,
			wantTool: "shell",
		},
		{
			name:     "reject with reason",
			content:  functionResponseContent(map[string]any{"decision": "reject", "reason": "nope"}),
			wantType: runtime.ResumeTypeReject,
			wantMsg:  "nope",
		},
		{
			name:     "unknown decision rejects",
			content:  functionResponseContent(map[string]any{"decision": "maybe"}),
			wantType: runtime.ResumeTypeReject,
			wantMsg:  "unrecognised approval decision: maybe",
		},
		{
			name:     "missing response rejects",
			content:  &genai.Content{Parts: []*genai.Part{{Text: "free text"}}},
			wantType: runtime.ResumeTypeReject,
			wantMsg:  "no approval decision provided",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := resumeDecision(tc.content, run)
			assert.Equal(t, tc.wantType, req.Type)
			if tc.wantTool != "" {
				assert.Equal(t, tc.wantTool, req.ToolName)
			}
			if tc.wantMsg != "" {
				assert.Equal(t, tc.wantMsg, req.Reason)
			}
		})
	}
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

	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	// Strict always asks for confirmation, so the call reaches askUser (where
	// the tool-approval switch lives) rather than being auto-allowed or denied
	// by the mode itself.
	e, err := newServer(t.Context(), tm, "test.yaml", "root", store, newTestTaskDeps(t),
		servesafety.Resolved{Policy: session.SafetyPolicyStrict}, testWorkspaceRoot, ln.Addr().String(),
		RunOptions{ToolApproval: toolApproval})
	require.NoError(t, err)
	go func() { _ = e.Server.Serve(ln) }()
	t.Cleanup(func() { require.NoError(t, e.Server.Close()) })

	client, err := a2aclient.NewFromEndpoints(t.Context(), []*a2a.AgentInterface{
		a2a.NewAgentInterface(fmt.Sprintf("http://%s/invoke", ln.Addr().String()), a2a.TransportProtocolJSONRPC),
	})
	require.NoError(t, err)
	return client
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
