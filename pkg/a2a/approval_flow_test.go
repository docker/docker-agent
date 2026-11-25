package a2a

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dagent "github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

func (p *sequentialMockProvider) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.next
}

func approvalTeamWith(tool tools.Tool, streams ...chat.MessageStream) (*team.Team, *sequentialMockProvider) {
	provider := &sequentialMockProvider{id: modelsdev.NewID("test", "mock-model"), streams: streams}
	root := dagent.New("root", "You are a test agent", dagent.WithModel(provider), dagent.WithTools(tool))
	return team.New(team.WithAgents(root)), provider
}

func textThenToolCallStream(text, toolName string) chat.MessageStream {
	return &mockStream{responses: []chat.MessageStreamResponse{
		{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{Content: text}}}},
		{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{ToolCalls: []tools.ToolCall{{
			ID:       "tool-call",
			Type:     "function",
			Function: tools.FunctionCall{Name: toolName, Arguments: "{}"},
		}}}}}},
	}}
}

func sendText(t *testing.T, client *a2aclient.Client, contextID, text string) a2a.SendMessageResult {
	t.Helper()
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	msg.ContextID = contextID
	res, err := client.SendMessage(t.Context(), &a2a.SendMessageRequest{Message: msg})
	require.NoError(t, err)
	return res
}

func requireInvalidParams(t *testing.T, err error, reason string) {
	t.Helper()
	require.ErrorIs(t, err, a2a.ErrInvalidParams)
	var a2aErr *a2a.Error
	require.ErrorAs(t, err, &a2aErr)
	assert.Equal(t, reason, a2aErr.Details["reason"])
}

// A new task in the context of a task waiting for approval can neither answer
// nor strand that approval; the waiting task still resumes afterwards.
func TestServer_ToolApproval_OtherTaskInContextCannotTakeApproval(t *testing.T) {
	t.Parallel()

	var executions atomic.Int32
	client := startToolApprovalServer(t, approvalTeam(&executions), session.NewInMemorySessionStore(), true)

	task, callID := requireInputRequired(t, sendText(t, client, "", "run the tool"))
	wantReason := "task " + string(task.ID) + " is waiting for tool approval in this context; reply with its taskId or cancel it"

	text := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("reject it"))
	text.ContextID = task.ContextID
	_, err := client.SendMessage(t.Context(), &a2a.SendMessageRequest{Message: text})
	requireInvalidParams(t, err, wantReason)

	answer := answerToolCall(task, callID, map[string]any{"decision": "approve"})
	answer.TaskID = ""
	_, err = client.SendMessage(t.Context(), &a2a.SendMessageRequest{Message: answer})
	requireInvalidParams(t, err, wantReason)

	res, err := client.SendMessage(t.Context(), &a2a.SendMessageRequest{
		Message: answerToolCall(task, callID, map[string]any{"decision": "approve"}),
	})
	require.NoError(t, err)
	done, ok := res.(*a2a.Task)
	require.True(t, ok, "expected a task, got %T", res)
	assert.Equal(t, a2a.TaskStateCompleted, done.Status.State)
	assert.Equal(t, int32(1), executions.Load())
}

// A reply to a task whose parked run is gone (lost before it could be failed)
// fails the task instead of starting a new model turn.
func TestServer_ToolApproval_LateReplyFailsWithoutModelCall(t *testing.T) {
	t.Parallel()

	var executions atomic.Int32
	tm, provider := approvalTeamWith(approvalCanaryTool(&executions), toolCallStream("approval_canary"), stopStream())
	reg := newApprovalRegistry(t.Context(), true)
	client, _ := startApprovalServer(t, tm, session.NewInMemorySessionStore(), RunOptions{ToolApproval: true, approvals: reg})

	task, callID := requireInputRequired(t, sendText(t, client, "", "run the tool"))
	callsBefore := provider.calls()
	lost, ok := reg.take(task.ID, callID)
	require.True(t, ok)
	lost.cancelAndDrain()

	res, err := client.SendMessage(t.Context(), &a2a.SendMessageRequest{
		Message: answerToolCall(task, callID, map[string]any{"decision": "approve"}),
	})
	require.NoError(t, err)
	failed, ok := res.(*a2a.Task)
	require.True(t, ok, "expected a task, got %T", res)
	assert.Equal(t, a2a.TaskStateFailed, failed.Status.State)
	assert.Equal(t, callsBefore, provider.calls(), "a late reply must not call the model")
	assert.Zero(t, executions.Load(), "a late reply must not run the tool")
}

// pausedTaskWithWebhook parks a task on approval with a webhook registered for
// it and returns the task and the events the webhook receives.
func pausedTaskWithWebhook(t *testing.T, client *a2aclient.Client) (*a2a.Task, string, <-chan a2a.Event) {
	t.Helper()
	received := make(chan a2a.Event, 16)
	webhook := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		var sr a2a.StreamResponse
		if assert.NoError(t, json.NewDecoder(r.Body).Decode(&sr)) {
			received <- sr.Event
		}
	}))
	t.Cleanup(webhook.Close)

	res, err := client.SendMessage(t.Context(), &a2a.SendMessageRequest{
		Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("run the tool")),
		Config:  &a2a.SendMessageConfig{PushConfig: &a2a.PushConfig{URL: webhook.URL, Token: "tok"}},
	})
	require.NoError(t, err)
	task, callID := requireInputRequired(t, res)
	return task, callID, received
}

func statusText(task *a2a.Task) string {
	if task.Status.Message == nil {
		return ""
	}
	var text strings.Builder
	for _, part := range task.Status.Message.Parts {
		text.WriteString(part.Text())
	}
	return text.String()
}

// An approval the janitor evicts fails its task right away: polling clients and
// webhooks learn it without having to reply first.
func TestServer_ToolApproval_ExpiredApprovalFailsTask(t *testing.T) {
	t.Parallel()

	var executions atomic.Int32
	tm, provider := approvalTeamWith(approvalCanaryTool(&executions), toolCallStream("approval_canary"), stopStream())
	reg := newApprovalRegistry(t.Context(), true)
	client, _ := startApprovalServer(t, tm, session.NewInMemorySessionStore(), RunOptions{ToolApproval: true, approvals: reg})

	task, callID, received := pausedTaskWithWebhook(t, client)
	callsBefore := provider.calls()
	reg.evictExpired(time.Now().Add(2 * defaultApprovalTTL))

	got, err := client.GetTask(t.Context(), &a2a.GetTaskRequest{ID: task.ID})
	require.NoError(t, err)
	assert.Equal(t, a2a.TaskStateFailed, got.Status.State)
	assert.Equal(t, approvalExpiredReason, statusText(got))
	waitPushState(t, received, a2a.TaskStateFailed)

	_, _ = client.SendMessage(t.Context(), &a2a.SendMessageRequest{
		Message: answerToolCall(task, callID, map[string]any{"decision": "approve"}),
	})
	assert.Equal(t, callsBefore, provider.calls(), "a late reply must not call the model")
	assert.Zero(t, executions.Load(), "a late reply must not run the tool")
}

// Shutdown fails paused tasks and notifies their webhooks before returning.
func TestServer_ToolApproval_ShutdownFailsPausedTask(t *testing.T) {
	t.Parallel()

	var executions atomic.Int32
	serverCtx, stopServer := context.WithCancel(t.Context())
	deps := newTestTaskDeps(t)
	reg := newApprovalRegistry(serverCtx, true)
	client, _ := startApprovalServerWith(t, serverCtx, approvalTeam(&executions), session.NewInMemorySessionStore(), deps, RunOptions{ToolApproval: true, approvals: reg})

	task, _, received := pausedTaskWithWebhook(t, client)
	stopServer()
	reg.close()

	ctx, _ := a2asrv.NewCallContext(t.Context(), nil)
	stored, err := deps.tasks.Get(ctx, task.ID)
	require.NoError(t, err)
	assert.Equal(t, a2a.TaskStateFailed, stored.Task.Status.State)
	assert.Equal(t, approvalShutdownReason, statusText(stored.Task))
	waitPushState(t, received, a2a.TaskStateFailed)
	assert.Zero(t, executions.Load())
}

// Cancelling a task while its approved tool runs cancels the tool.
func TestServer_ToolApproval_CancelStopsApprovedTool(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	stopped := make(chan struct{})
	tool := tools.Tool{
		Name:       "approval_canary",
		Parameters: map[string]any{},
		Handler: func(ctx context.Context, _ tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
			close(started)
			<-ctx.Done()
			close(stopped)
			return nil, ctx.Err()
		},
	}
	tm, _ := approvalTeamWith(tool, toolCallStream("approval_canary"), stopStream())
	client, _ := startApprovalServer(t, tm, session.NewInMemorySessionStore(), RunOptions{ToolApproval: true})

	task, callID := requireInputRequired(t, sendText(t, client, "", "run the tool"))

	go func() {
		_, _ = client.SendMessage(context.WithoutCancel(t.Context()), &a2a.SendMessageRequest{
			Message: answerToolCall(task, callID, map[string]any{"decision": "approve"}),
		})
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("approved tool did not start")
	}

	cancelCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	canceled, err := client.CancelTask(cancelCtx, &a2a.CancelTaskRequest{ID: task.ID})
	select {
	case <-stopped:
	case <-cancelCtx.Done():
		t.Fatal("cancelling the task did not cancel the running tool")
	}
	require.NoError(t, err)
	assert.Equal(t, a2a.TaskStateCanceled, canceled.Status.State)
}

// approve-in-context survives the session reload of the next turn.
func TestServer_ToolApproval_ApproveInContextPersistsAcrossTurns(t *testing.T) {
	t.Parallel()

	var executions atomic.Int32
	tm, _ := approvalTeamWith(approvalCanaryTool(&executions),
		toolCallStream("approval_canary"), stopStream(),
		toolCallStream("approval_canary"), stopStream())
	client, _ := startApprovalServer(t, tm, newA2AResumeStore(t, "sqlite"), RunOptions{ToolApproval: true})

	task, callID := requireInputRequired(t, sendText(t, client, "", "run the tool"))
	res, err := client.SendMessage(t.Context(), &a2a.SendMessageRequest{
		Message: answerToolCall(task, callID, map[string]any{"decision": "approve-in-context"}),
	})
	require.NoError(t, err)
	require.Equal(t, a2a.TaskStateCompleted, res.(*a2a.Task).Status.State)

	second, ok := sendText(t, client, task.ContextID, "run it again").(*a2a.Task)
	require.True(t, ok)
	assert.Equal(t, a2a.TaskStateCompleted, second.Status.State, "the approved tool must not ask again")
	assert.Equal(t, int32(2), executions.Load())
}

// approve covers the pending call only: the next call asks again.
func TestServer_ToolApproval_ApproveIsOneShot(t *testing.T) {
	t.Parallel()

	var executions atomic.Int32
	tm, _ := approvalTeamWith(approvalCanaryTool(&executions),
		toolCallStream("approval_canary"), stopStream(),
		toolCallStream("approval_canary"), stopStream())
	client, _ := startApprovalServer(t, tm, newA2AResumeStore(t, "sqlite"), RunOptions{ToolApproval: true})

	task, callID := requireInputRequired(t, sendText(t, client, "", "run the tool"))
	res, err := client.SendMessage(t.Context(), &a2a.SendMessageRequest{
		Message: answerToolCall(task, callID, map[string]any{"decision": "approve"}),
	})
	require.NoError(t, err)
	require.Equal(t, a2a.TaskStateCompleted, res.(*a2a.Task).Status.State)

	requireInputRequired(t, sendText(t, client, task.ContextID, "run it again"))
	assert.Equal(t, int32(1), executions.Load())
}

// approve-in-context on a shell call grants that command, as the TUI does, not
// every shell command.
func TestServer_ToolApproval_ApproveInContextScopesShellToCommand(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		next    string
		allowed bool
	}{
		{next: `{"cmd":"ls"}`, allowed: true},
		{next: `{"cmd":"ls -la /tmp"}`, allowed: true},
		{next: `{"cmd":"lsblk"}`},
		{next: `{"cmd":"rm -rf /tmp/x"}`},
		{next: `{"cmd":"ls && rm -rf ~"}`},
	} {
		t.Run(tc.next, func(t *testing.T) {
			t.Parallel()

			var executions atomic.Int32
			shell := approvalCanaryTool(&executions)
			shell.Name = "shell"
			tm, _ := approvalTeamWith(shell,
				toolCallStreamArgs("shell", `{"cmd":"ls"}`), stopStream(),
				toolCallStreamArgs("shell", tc.next), stopStream())
			client, _ := startApprovalServer(t, tm, newA2AResumeStore(t, "sqlite"), RunOptions{ToolApproval: true})

			task, callID := requireInputRequired(t, sendText(t, client, "", "list files"))
			var grant map[string]any
			for _, part := range task.Status.Message.Parts {
				if m, ok := part.Meta()[toolApprovalExtensionURI].(map[string]any); ok {
					grant = m
				}
			}
			require.NotNil(t, grant)
			assert.Equal(t, "shell:cmd=ls*", grant["grant"])
			assert.Equal(t, "allow ls* in this context", grant["grantLabel"])

			answer := answerToolCall(task, callID, map[string]any{"decision": "approve-in-context"})
			res, err := client.SendMessage(t.Context(), &a2a.SendMessageRequest{Message: answer})
			require.NoError(t, err)
			require.Equal(t, a2a.TaskStateCompleted, res.(*a2a.Task).Status.State)

			next := sendText(t, client, task.ContextID, "again")
			if tc.allowed {
				assert.Equal(t, a2a.TaskStateCompleted, next.(*a2a.Task).Status.State)
				assert.Equal(t, int32(2), executions.Load())
				return
			}
			requireInputRequired(t, next)
			assert.Equal(t, int32(1), executions.Load())
		})
	}
}

// Text the model streams before asking for approval stays in the artifacts.
func TestServer_ToolApproval_TextBeforeApprovalIsKept(t *testing.T) {
	t.Parallel()

	var executions atomic.Int32
	tm, _ := approvalTeamWith(approvalCanaryTool(&executions),
		textThenToolCallStream("Checking first.", "approval_canary"), answerStream("All done."))
	client, _ := startApprovalServer(t, tm, session.NewInMemorySessionStore(), RunOptions{ToolApproval: true})

	task, callID := requireInputRequired(t, sendText(t, client, "", "run the tool"))
	res, err := client.SendMessage(t.Context(), &a2a.SendMessageRequest{
		Message: answerToolCall(task, callID, map[string]any{"decision": "approve"}),
	})
	require.NoError(t, err)
	done := res.(*a2a.Task)
	require.Equal(t, a2a.TaskStateCompleted, done.Status.State)

	var text strings.Builder
	for _, artifact := range done.Artifacts {
		for _, part := range artifact.Parts {
			text.WriteString(part.Text())
		}
	}
	assert.Contains(t, text.String(), "Checking first.")
	assert.Contains(t, text.String(), "All done.")
}

// An invalid decision is rejected with -32602 and leaves the task waiting with
// its history unchanged.
func TestServer_ToolApproval_InvalidDecisionKeepsTaskWaiting(t *testing.T) {
	t.Parallel()

	var executions atomic.Int32
	client := startToolApprovalServer(t, approvalTeam(&executions), session.NewInMemorySessionStore(), true)

	task, callID := requireInputRequired(t, sendText(t, client, "", "run the tool"))
	before, err := client.GetTask(t.Context(), &a2a.GetTaskRequest{ID: task.ID})
	require.NoError(t, err)

	_, err = client.SendMessage(t.Context(), &a2a.SendMessageRequest{
		Message: answerToolCall(task, callID, map[string]any{"decision": "maybe"}),
	})
	requireInvalidParams(t, err, "decision 'maybe' not allowed; expected [approve, approve-in-context, reject]")

	after, err := client.GetTask(t.Context(), &a2a.GetTaskRequest{ID: task.ID})
	require.NoError(t, err)
	assert.Equal(t, a2a.TaskStateInputRequired, after.Status.State)
	assert.Len(t, after.History, len(before.History), "a rejected reply must not be recorded")

	res, err := client.SendMessage(t.Context(), &a2a.SendMessageRequest{
		Message: answerToolCall(task, callID, map[string]any{"decision": "approve"}),
	})
	require.NoError(t, err)
	assert.Equal(t, a2a.TaskStateCompleted, res.(*a2a.Task).Status.State)
	assert.Equal(t, int32(1), executions.Load())
}

func TestServer_ToolApproval_Extension(t *testing.T) {
	t.Parallel()

	card := func(t *testing.T, addr string) *a2a.AgentCard {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/.well-known/agent-card.json", http.NoBody)
		require.NoError(t, err)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		var c a2a.AgentCard
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&c))
		return &c
	}

	t.Run("disabled", func(t *testing.T) {
		t.Parallel()
		var executions atomic.Int32
		_, addr := startApprovalServer(t, approvalTeam(&executions), session.NewInMemorySessionStore(), RunOptions{})
		assert.Empty(t, card(t, addr).Capabilities.Extensions)
	})

	t.Run("enabled", func(t *testing.T) {
		t.Parallel()
		var executions atomic.Int32
		client, addr := startApprovalServer(t, approvalTeam(&executions), session.NewInMemorySessionStore(), RunOptions{ToolApproval: true})

		exts := card(t, addr).Capabilities.Extensions
		require.Len(t, exts, 1)
		assert.Equal(t, toolApprovalExtensionURI, exts[0].URI)
		assert.False(t, exts[0].Required)

		task, _ := requireInputRequired(t, sendText(t, client, "", "run the tool"))
		var meta map[string]any
		for _, part := range task.Status.Message.Parts {
			if m, ok := part.Meta()[toolApprovalExtensionURI].(map[string]any); ok {
				meta = m
			}
		}
		require.NotNil(t, meta, "the pending call must carry the extension metadata")
		assert.Equal(t, `Tool "approval_canary" requires approval`, meta["prompt"])
		schema, ok := meta["responseSchema"].(map[string]any)
		require.True(t, ok)
		decision := schema["properties"].(map[string]any)["decision"].(map[string]any)
		assert.Equal(t, []any{"approve", "approve-in-context", "reject"}, decision["enum"])
		assert.Equal(t, "approval_canary", meta["grant"])
		assert.Equal(t, "allow approval_canary in this context", meta["grantLabel"])
	})
}

// approve-in-context survives a server restart: the grant lives in the session store.
func TestServer_ToolApproval_ApproveToolSurvivesRestart(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	openStore := func() session.Store {
		store, err := sqlitestore.New(t.Context(), dbPath)
		require.NoError(t, err)
		return store
	}

	var executions atomic.Int32
	tm, _ := approvalTeamWith(approvalCanaryTool(&executions), toolCallStream("approval_canary"), stopStream())
	serverCtx, stopServer := context.WithCancel(t.Context())
	store := openStore()
	client, _ := startApprovalServerCtx(t, serverCtx, tm, store, RunOptions{ToolApproval: true})

	task, callID := requireInputRequired(t, sendText(t, client, "", "run the tool"))
	res, err := client.SendMessage(t.Context(), &a2a.SendMessageRequest{
		Message: answerToolCall(task, callID, map[string]any{"decision": "approve-in-context"}),
	})
	require.NoError(t, err)
	require.Equal(t, a2a.TaskStateCompleted, res.(*a2a.Task).Status.State)
	stopServer()
	require.NoError(t, store.Close())

	tm2, _ := approvalTeamWith(approvalCanaryTool(&executions), toolCallStream("approval_canary"), stopStream())
	restarted := openStore()
	t.Cleanup(func() { require.NoError(t, restarted.Close()) })
	client2, _ := startApprovalServer(t, tm2, restarted, RunOptions{ToolApproval: true})

	second, ok := sendText(t, client2, task.ContextID, "run it again").(*a2a.Task)
	require.True(t, ok)
	assert.Equal(t, a2a.TaskStateCompleted, second.Status.State, "the grant must survive a restart")
	assert.Equal(t, int32(2), executions.Load())
}

func toolCallStreamArgs(name, args string) chat.MessageStream {
	return &mockStream{responses: []chat.MessageStreamResponse{{
		Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{ToolCalls: []tools.ToolCall{{
			ID:       "tool-call",
			Type:     "function",
			Function: tools.FunctionCall{Name: name, Arguments: args},
		}}}}},
	}}}
}
