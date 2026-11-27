package a2a

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
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

// Shutdown landing between the park and the persisted INPUT_REQUIRED still
// fails the task and notifies its webhook once the pause is published.
func TestServer_ToolApproval_ShutdownDuringPauseHandoff(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		at   approvalHookPoint
	}{
		{"before park", hookBeforePark},
		{"after park", hookAfterPark},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var executions atomic.Int32
			deps := newTestTaskDeps(t)
			// The registry shuts down while the HTTP server keeps serving the task.
			reg := newApprovalRegistry(t.Context(), true)
			paused := make(chan a2a.TaskID, 1)
			release := make(chan struct{})
			reg.hook = func(point approvalHookPoint, id a2a.TaskID) {
				if point == tc.at {
					paused <- id
					<-release
				}
			}
			client, _ := startApprovalServerWith(t, t.Context(), approvalTeam(&executions), session.NewInMemorySessionStore(), deps, RunOptions{ToolApproval: true, approvals: reg})

			received := make(chan a2a.Event, 16)
			webhook := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				var sr a2a.StreamResponse
				if assert.NoError(t, json.NewDecoder(r.Body).Decode(&sr)) {
					received <- sr.Event
				}
			}))
			t.Cleanup(webhook.Close)
			go func() {
				_, _ = client.SendMessage(context.WithoutCancel(t.Context()), &a2a.SendMessageRequest{
					Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("run the tool")),
					Config:  &a2a.SendMessageConfig{PushConfig: &a2a.PushConfig{URL: webhook.URL, Token: "tok"}},
				})
			}()
			var taskID a2a.TaskID
			select {
			case taskID = <-paused:
			case <-time.After(10 * time.Second):
				t.Fatal("the run did not reach the pause")
			}

			closed := make(chan struct{})
			go func() {
				reg.close()
				close(closed)
			}()
			select {
			case <-reg.done:
			case <-time.After(10 * time.Second):
				t.Fatal("shutdown did not release the parked runs")
			}
			close(release)
			select {
			case <-closed:
			case <-time.After(10 * time.Second):
				t.Fatal("shutdown did not stop the runtime")
			}

			ctx, _ := a2asrv.NewCallContext(t.Context(), nil)
			require.Eventually(t, func() bool {
				stored, err := deps.tasks.Get(ctx, taskID)
				return err == nil && stored.Task.Status.State == a2a.TaskStateFailed
			}, 10*time.Second, 5*time.Millisecond, "the task must not stay waiting for an approval nobody can resume")
			stored, err := deps.tasks.Get(ctx, taskID)
			require.NoError(t, err)
			assert.Equal(t, approvalShutdownReason, statusText(stored.Task))
			waitPushState(t, received, a2a.TaskStateFailed)
			assert.Zero(t, executions.Load())
		})
	}
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
				shellCallStream(`{"cmd":"ls"}`), stopStream(),
				shellCallStream(tc.next), stopStream())
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

	_, err = client.SendMessage(t.Context(), &a2a.SendMessageRequest{
		Message: answerToolCall(task, callID, map[string]any{"decision": "reject", "reason": 123}),
	})
	requireInvalidParams(t, err, "reason must be a string")

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
		assert.NotContains(t, meta, "rawArguments", "decoded args are already in data.args")
	})
}

// Arguments that don't decode as an object are shown verbatim: a handler may
// repair and run them (e.g. edit_file), so the approver must see them.
func TestServer_ToolApproval_MalformedArgsAreShown(t *testing.T) {
	t.Parallel()

	const raw = `{"path": "a.txt", "edits": [{"oldText": "a", "newText": "b"}]`
	var executions atomic.Int32
	tool := approvalCanaryTool(&executions)
	tm, _ := approvalTeamWith(tool, &mockStream{responses: []chat.MessageStreamResponse{{
		Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{ToolCalls: []tools.ToolCall{{
			ID:       "tool-call",
			Type:     "function",
			Function: tools.FunctionCall{Name: tool.Name, Arguments: raw},
		}}}}},
	}}}, stopStream())
	client := startToolApprovalServer(t, tm, session.NewInMemorySessionStore(), true)

	task, _ := requireInputRequired(t, sendText(t, client, "", "run the tool"))
	var meta map[string]any
	for _, part := range task.Status.Message.Parts {
		if m, ok := part.Meta()[toolApprovalExtensionURI].(map[string]any); ok {
			meta = m
			assert.NotContains(t, part.Data(), "args")
		}
	}
	require.NotNil(t, meta)
	assert.Equal(t, raw, meta["rawArguments"]) //nolint:testifylint // raw is deliberately invalid JSON.
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

func shellCallStream(args string) chat.MessageStream {
	return &mockStream{responses: []chat.MessageStreamResponse{{
		Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{ToolCalls: []tools.ToolCall{{
			ID:       "tool-call",
			Type:     "function",
			Function: tools.FunctionCall{Name: "shell", Arguments: args},
		}}}}},
	}}}
}

// A CancelTask racing the pause must leave a terminal task and no parked run,
// whichever side of the park and of the INPUT_REQUIRED publication it lands.
func TestServer_ToolApproval_CancelDuringPauseHandoff(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		at   approvalHookPoint
	}{
		{"before park", hookBeforePark},
		{"after park", hookAfterPark},
		{"after publish", hookAfterFinalEvent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var executions atomic.Int32
			reg := newApprovalRegistry(t.Context(), true)
			paused := make(chan a2a.TaskID, 1)
			release := make(chan struct{})
			finalHandled := make(chan struct{}, 1)
			reg.hook = func(point approvalHookPoint, id a2a.TaskID) {
				switch point {
				case tc.at:
					paused <- id
					<-release
				case hookBeforeCancelEvent:
					// Let the execution reach its final event before ADK emits
					// CANCELED: the order a2a-go cannot recover from on its own.
					close(release)
					if tc.at != hookAfterFinalEvent {
						select {
						case <-finalHandled:
						case <-time.After(10 * time.Second):
						}
					}
				}
				if point == hookAfterFinalEvent {
					select {
					case finalHandled <- struct{}{}:
					default:
					}
				}
			}
			client, _ := startApprovalServer(t, approvalTeam(&executions), session.NewInMemorySessionStore(), RunOptions{ToolApproval: true, approvals: reg})

			go func() {
				_, _ = client.SendMessage(context.WithoutCancel(t.Context()), &a2a.SendMessageRequest{
					Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("run the tool")),
				})
			}()
			var taskID a2a.TaskID
			select {
			case taskID = <-paused:
			case <-time.After(10 * time.Second):
				t.Fatal("the run did not reach the pause")
			}

			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			// The SDK stores the first task event asynchronously; a client can
			// only cancel a task it has seen.
			require.Eventually(t, func() bool {
				_, err := client.GetTask(ctx, &a2a.GetTaskRequest{ID: taskID})
				return err == nil
			}, 10*time.Second, 5*time.Millisecond)
			canceled, err := client.CancelTask(ctx, &a2a.CancelTaskRequest{ID: taskID})
			require.NoError(t, err, "the cancel request canceled the task")
			assert.Equal(t, a2a.TaskStateCanceled, canceled.Status.State)
			assert.Equal(t, taskID, canceled.ID)

			got, err := client.GetTask(ctx, &a2a.GetTaskRequest{ID: taskID})
			require.NoError(t, err)
			assert.Equal(t, a2a.TaskStateCanceled, got.Status.State)
			_, parked := reg.pending(taskID)
			assert.False(t, parked, "no run may stay parked for a canceled task")
			_, held := reg.pendingInContext(got.ContextID)
			assert.False(t, held, "a canceled task must not hold its context")
			reg.mu.Lock()
			idle := reg.idle
			reg.mu.Unlock()
			select {
			case <-idle:
			case <-ctx.Done():
				t.Fatal("the runtime of the canceled task is still running")
			}
			assert.Zero(t, executions.Load())
		})
	}
}

// Every CancelTask request that loses the race to INPUT_REQUIRED reports the
// CANCELED task its cleanup persisted, whether a2a-go failed the cancel with
// ErrTaskNotCancelable or on the ended execution's closed event pipe, and
// however many requests share the cancellation.
func TestServer_ToolApproval_CancelAfterPublishReportsCanceled(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		pipeClosed bool
		callers    int
	}{
		{"pipe closed before the cancel write", true, 1},
		{"callers share the cancellation", false, 2},
		{"callers share a cancellation on a closed pipe", true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()

			var executions atomic.Int32
			reg := newApprovalRegistry(t.Context(), true)
			paused := make(chan a2a.TaskID, 1)
			release := make(chan struct{})
			releaseOnce := sync.OnceFunc(func() { close(release) })
			var client *a2aclient.Client
			var pending atomic.Pointer[pausedRun]
			reg.hook = func(point approvalHookPoint, id a2a.TaskID) {
				switch point {
				case hookAfterFinalEvent:
					paused <- id
					<-release
				case hookBeforeCancelEvent:
					if tc.callers > 1 {
						assert.Eventually(t, func() bool {
							reg.mu.Lock()
							defer reg.mu.Unlock()
							return reg.cancelCalls[id] == tc.callers
						}, 10*time.Second, time.Millisecond)
						// a2a-go joins them to this cancellation without a signal.
						time.Sleep(50 * time.Millisecond) //nolint:forbidigo // no observable join point
					}
					if !tc.pipeClosed {
						// The execution keeps its pipe until the cancel event is written
						// (hookAfterCancelEvent): a2a-go reports INPUT_REQUIRED.
						return
					}
					releaseOnce()
					// a2a-go answers a follow-up with "cancelation is in progress"
					// only once the execution is gone and its pipe closed.
					run := pending.Load()
					reply := answerToolCall(&a2a.Task{ID: id, ContextID: run.contextID}, run.callID, map[string]any{"decision": "approve"})
					assert.Eventually(t, func() bool {
						_, err := client.SendMessage(ctx, &a2a.SendMessageRequest{Message: reply})
						return err != nil && strings.Contains(err.Error(), "cancelation is in progress")
					}, 10*time.Second, 5*time.Millisecond)
				case hookAfterCancelEvent:
					if !tc.pipeClosed {
						releaseOnce()
					}
				}
			}
			client, _ = startApprovalServer(t, approvalTeam(&executions), session.NewInMemorySessionStore(), RunOptions{ToolApproval: true, approvals: reg})

			go func() {
				_, _ = client.SendMessage(context.WithoutCancel(t.Context()), &a2a.SendMessageRequest{
					Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("run the tool")),
				})
			}()
			var taskID a2a.TaskID
			select {
			case taskID = <-paused:
			case <-ctx.Done():
				t.Fatal("the run did not publish INPUT_REQUIRED")
			}
			run, ok := reg.pending(taskID)
			require.True(t, ok)
			pending.Store(run)
			require.Eventually(t, func() bool {
				_, err := client.GetTask(ctx, &a2a.GetTaskRequest{ID: taskID})
				return err == nil
			}, 10*time.Second, 5*time.Millisecond)

			type result struct {
				task *a2a.Task
				err  error
			}
			results := make(chan result, tc.callers)
			for range tc.callers {
				go func() {
					task, err := client.CancelTask(ctx, &a2a.CancelTaskRequest{ID: taskID})
					results <- result{task, err}
				}()
			}
			for range tc.callers {
				res := <-results
				require.NoError(t, res.err, "every cancel request canceled the task")
				assert.Equal(t, a2a.TaskStateCanceled, res.task.Status.State)
				assert.Equal(t, taskID, res.task.ID)
			}

			got, err := client.GetTask(ctx, &a2a.GetTaskRequest{ID: taskID})
			require.NoError(t, err)
			assert.Equal(t, a2a.TaskStateCanceled, got.Status.State)
			_, parked := reg.pending(taskID)
			assert.False(t, parked)
			reg.mu.Lock()
			assert.Empty(t, reg.canceled, "the fallback is dropped once its requests end")
			assert.Empty(t, reg.cancelCalls)
			reg.mu.Unlock()
			assert.Zero(t, executions.Load())
		})
	}
}

// A JSON null part reaches the interceptor as a nil *Part: the reply is
// rejected with -32602 instead of panicking in ADK's converter.
func TestServer_ToolApproval_NullPartKeepsTaskWaiting(t *testing.T) {
	t.Parallel()

	var executions atomic.Int32
	client, addr := startApprovalServer(t, approvalTeam(&executions), session.NewInMemorySessionStore(), RunOptions{ToolApproval: true})

	task, callID := requireInputRequired(t, sendText(t, client, "", "run the tool"))
	before, err := client.GetTask(t.Context(), &a2a.GetTaskRequest{ID: task.ID})
	require.NoError(t, err)

	valid, err := json.Marshal(answerToolCall(task, callID, map[string]any{"decision": "approve"}))
	require.NoError(t, err)
	var msg map[string]any
	require.NoError(t, json.Unmarshal(valid, &msg))
	validPart := msg["parts"].([]any)[0]

	for _, parts := range [][]any{{nil}, {nil, validPart}} {
		msg["parts"] = parts
		body, err := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "SendMessage",
			"params": map[string]any{"message": msg},
		})
		require.NoError(t, err)
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+addr+"/invoke", strings.NewReader(string(body)))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		var rpc struct {
			Error *struct {
				Code int `json:"code"`
			} `json:"error"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&rpc))
		resp.Body.Close()
		require.NotNil(t, rpc.Error, "parts %v must be rejected", parts)
		assert.Equal(t, -32602, rpc.Error.Code)
	}

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

// A command call with no usable command word offers no approve-in-context: it
// would be stored as a whole-tool grant allowing every later command.
func TestServer_ToolApproval_UnscopableCommandLeavesNoToolGrant(t *testing.T) {
	t.Parallel()

	var executions atomic.Int32
	shell := approvalCanaryTool(&executions)
	shell.Name = "shell"
	tm, _ := approvalTeamWith(shell,
		shellCallStream(`{"cmd":""}`), stopStream(),
		shellCallStream(`{"cmd":"rm -rf /tmp/x"}`), stopStream())
	store := newA2AResumeStore(t, "sqlite")
	client, _ := startApprovalServer(t, tm, store, RunOptions{ToolApproval: true})

	task, callID := requireInputRequired(t, sendText(t, client, "", "run something"))
	var meta map[string]any
	for _, part := range task.Status.Message.Parts {
		if m, ok := part.Meta()[toolApprovalExtensionURI].(map[string]any); ok {
			meta = m
		}
	}
	require.NotNil(t, meta)
	assert.NotContains(t, meta, "grant")
	assert.NotContains(t, meta, "grantLabel")
	decision := meta["responseSchema"].(map[string]any)["properties"].(map[string]any)["decision"].(map[string]any)
	assert.Equal(t, []any{"approve", "reject"}, decision["enum"])

	_, err := client.SendMessage(t.Context(), &a2a.SendMessageRequest{
		Message: answerToolCall(task, callID, map[string]any{"decision": "approve-in-context"}),
	})
	requireInvalidParams(t, err, "decision 'approve-in-context' not allowed; expected [approve, reject]")

	res, err := client.SendMessage(t.Context(), &a2a.SendMessageRequest{
		Message: answerToolCall(task, callID, map[string]any{"decision": "approve"}),
	})
	require.NoError(t, err)
	require.Equal(t, a2a.TaskStateCompleted, res.(*a2a.Task).Status.State)

	stored, err := store.GetSessionByOrigin(t.Context(), task.ContextID, "a2a")
	require.NoError(t, err)
	if stored.Permissions != nil {
		assert.NotContains(t, stored.Permissions.Allow, "shell")
	}

	requireInputRequired(t, sendText(t, client, task.ContextID, "again"))
	assert.Equal(t, int32(1), executions.Load(), "a later command must ask again")
}
