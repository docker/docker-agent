package a2a

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/push"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/a2a/a2astore"
	"github.com/docker/docker-agent/pkg/a2a/pushsender"
	dagent "github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/servesafety"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

type sequentialMockProvider struct {
	id      modelsdev.ID
	streams []chat.MessageStream

	mu   sync.Mutex
	next int
}

func (p *sequentialMockProvider) ID() modelsdev.ID { return p.id }

func (p *sequentialMockProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.next >= len(p.streams) {
		return &mockStream{responses: []chat.MessageStreamResponse{{
			Choices: []chat.MessageStreamChoice{{Index: 0, FinishReason: chat.FinishReasonStop}},
		}}}, nil
	}
	stream := p.streams[p.next]
	p.next++
	return stream, nil
}

func (p *sequentialMockProvider) BaseConfig() base.Config { return base.Config{} }
func (p *sequentialMockProvider) MaxTokens() int          { return 0 }

func toolCallStream(name string) chat.MessageStream {
	return &mockStream{responses: []chat.MessageStreamResponse{{
		Choices: []chat.MessageStreamChoice{{
			Index: 0,
			Delta: chat.MessageDelta{ToolCalls: []tools.ToolCall{{
				ID:       "tool-call",
				Type:     "function",
				Function: tools.FunctionCall{Name: name, Arguments: "{}"},
			}}},
		}},
	}}}
}

func stopStream() chat.MessageStream {
	return &mockStream{responses: []chat.MessageStreamResponse{{
		Choices: []chat.MessageStreamChoice{{Index: 0, FinishReason: chat.FinishReasonStop}},
	}}}
}

func TestServer_ToolPolicyOverInvoke(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		policy   session.SafetyPolicy
		executed int32
	}{
		{name: "restricted", policy: session.SafetyPolicyRestricted},
		{name: "autonomous", policy: session.SafetyPolicyAutonomous, executed: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var executions atomic.Int32
			tool := tools.Tool{
				Name:        "unsafe_tool",
				Description: "test tool",
				Parameters:  map[string]any{},
				Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
					executions.Add(1)
					return tools.ResultSuccess("done"), nil
				},
			}
			provider := &sequentialMockProvider{
				id:      modelsdev.NewID("test", "mock-model"),
				streams: []chat.MessageStream{toolCallStream(tool.Name), stopStream()},
			}
			root := dagent.New("root", "You are a test agent", dagent.WithModel(provider), dagent.WithTools(tool))
			server := startInvokeServer(t, team.New(team.WithAgents(root)), session.NewInMemorySessionStore(), servesafety.Resolved{Policy: tc.policy})

			client, err := a2aclient.NewFromEndpoints(t.Context(), []*a2a.AgentInterface{
				a2a.NewAgentInterface(fmt.Sprintf("http://%s/invoke", server.Addr()), a2a.TransportProtocolJSONRPC),
			})
			require.NoError(t, err)
			_, err = client.SendMessage(t.Context(), &a2a.SendMessageRequest{
				Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("run the tool")),
			})
			require.NoError(t, err)
			require.Equal(t, tc.executed, executions.Load())
		})
	}
}

func TestServer_RejectsNonA2AContextCollision(t *testing.T) {
	t.Parallel()

	var executions atomic.Int32
	tool := tools.Tool{
		Name:        "collision_canary",
		Description: "test tool",
		Parameters:  map[string]any{},
		Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			executions.Add(1)
			return tools.ResultSuccess("done"), nil
		},
	}
	provider := &sequentialMockProvider{
		id:      modelsdev.NewID("test", "mock-model"),
		streams: []chat.MessageStream{toolCallStream(tool.Name), stopStream()},
	}
	root := dagent.New("root", "You are a test agent", dagent.WithModel(provider), dagent.WithTools(tool))
	store := session.NewInMemorySessionStore()
	existing := session.New(session.WithID("colliding-context"), session.WithOrigin("run"), session.WithTitle("private"))
	require.NoError(t, store.AddSession(t.Context(), existing))
	server := startInvokeServer(t, team.New(team.WithAgents(root)), store, servesafety.Resolved{Policy: session.SafetyPolicyAutonomous})

	client, err := a2aclient.NewFromEndpoints(t.Context(), []*a2a.AgentInterface{
		a2a.NewAgentInterface(fmt.Sprintf("http://%s/invoke", server.Addr()), a2a.TransportProtocolJSONRPC),
	})
	require.NoError(t, err)
	message := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("run the tool"))
	message.ContextID = "colliding-context"
	got, err := client.SendMessage(t.Context(), &a2a.SendMessageRequest{Message: message})
	require.NoError(t, err)
	task, ok := got.(*a2a.Task)
	require.True(t, ok)
	require.Equal(t, a2a.TaskStateFailed, task.Status.State)
	require.NotNil(t, task.Status.Message)
	require.Len(t, task.Status.Message.Parts, 1)
	failure := task.Status.Message.Parts[0]
	require.IsType(t, a2a.Text(""), failure.Content)
	require.Equal(t, "agent run failed: context ID is not available", failure.Text())
	require.Zero(t, executions.Load())

	stored, err := store.GetSession(t.Context(), existing.ID)
	require.NoError(t, err)
	require.Equal(t, "run", stored.Origin)
	require.Equal(t, "private", stored.Title)
}

// newTestTaskDeps returns in-memory task/push stores and a real sender that may
// reach loopback webhooks (httptest servers).
func newTestTaskDeps(t *testing.T) taskDeps {
	t.Helper()
	sender := pushsender.New(pushsender.Config{AllowPrivateNetworks: true})
	t.Cleanup(func() { _ = sender.Close(t.Context()) })
	return taskDeps{
		tasks:       taskstore.NewInMemory(&taskstore.InMemoryStoreConfig{Authenticator: a2asrv.NewTaskStoreAuthenticator()}),
		pushConfigs: push.NewInMemoryStore(),
		pushSender:  sender,
	}
}

func serveA2A(t *testing.T, deps taskDeps) (*echo.Echo, string) {
	t.Helper()
	root := dagent.New("root", "You are a test agent", dagent.WithModel(&sequentialMockProvider{
		id:      modelsdev.NewID("test", "mock-model"),
		streams: []chat.MessageStream{stopStream()},
	}))
	return serveTeam(t, team.New(team.WithAgents(root)), deps)
}

func serveTeam(t *testing.T, tm *team.Team, deps taskDeps) (*echo.Echo, string) {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	e, err := newServer(t.Context(), tm, "test.yaml", "root",
		session.NewInMemorySessionStore(), deps,
		servesafety.Resolved{Policy: session.SafetyPolicyAutonomous},
		testWorkspaceRoot, ln.Addr().String(), RunOptions{})
	require.NoError(t, err)
	go func() { _ = e.Server.Serve(ln) }()
	t.Cleanup(func() { _ = e.Server.Close() })
	return e, fmt.Sprintf("http://%s/invoke", ln.Addr().String())
}

func newA2AClient(t *testing.T, endpoint string) *a2aclient.Client {
	t.Helper()
	client, err := a2aclient.NewFromEndpoints(t.Context(), []*a2a.AgentInterface{
		a2a.NewAgentInterface(endpoint, a2a.TransportProtocolJSONRPC),
	})
	require.NoError(t, err)
	return client
}

type receivedPush struct {
	header http.Header
	event  a2a.Event
}

func TestServer_PushNotificationDelivered(t *testing.T) {
	t.Parallel()

	received := make(chan receivedPush, 16)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var sr a2a.StreamResponse
		if err := json.NewDecoder(r.Body).Decode(&sr); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		received <- receivedPush{header: r.Header.Clone(), event: sr.Event}
	}))
	t.Cleanup(webhook.Close)

	_, endpoint := serveA2A(t, newTestTaskDeps(t))
	client := newA2AClient(t, endpoint)

	// Registered with the message, i.e. before execution starts.
	_, err := client.SendMessage(t.Context(), &a2a.SendMessageRequest{
		Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hello")),
		Config: &a2a.SendMessageConfig{PushConfig: &a2a.PushConfig{
			URL:   webhook.URL + "/hook",
			Token: "notif-token",
			Auth:  &a2a.PushAuthInfo{Scheme: "Bearer", Credentials: "cb-secret"},
		}},
	})
	require.NoError(t, err)

	timeout := time.After(10 * time.Second)
	for {
		select {
		case got := <-received:
			assert.Equal(t, "notif-token", got.header.Get("A2A-Notification-Token"))
			assert.Equal(t, "Bearer cb-secret", got.header.Get("Authorization"))
			if pushState(got.event) == a2a.TaskStateCompleted {
				return
			}
		case <-timeout:
			t.Fatal("no completion push notification received")
		}
	}
}

func pushState(event a2a.Event) a2a.TaskState {
	switch e := event.(type) {
	case *a2a.Task:
		return e.Status.State
	case *a2a.TaskStatusUpdateEvent:
		return e.Status.State
	}
	return a2a.TaskStateUnspecified
}

func TestServer_PushConfigSurvivesRestart(t *testing.T) {
	t.Parallel()

	dbPath := t.TempDir()
	start := func() (*a2astore.Store, *echo.Echo, *a2aclient.Client) {
		store, err := a2astore.New(t.Context(), dbPath)
		require.NoError(t, err)
		deps := newTestTaskDeps(t)
		deps.tasks = store.Tasks(a2asrv.NewTaskStoreAuthenticator())
		deps.pushConfigs = store
		e, endpoint := serveA2A(t, deps)
		return store, e, newA2AClient(t, endpoint)
	}

	store1, server1, client1 := start()
	result, err := client1.SendMessage(t.Context(), &a2a.SendMessageRequest{
		Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hello")),
	})
	require.NoError(t, err)
	task, ok := result.(*a2a.Task)
	require.True(t, ok, "autonomous run should return a Task")

	saved, err := client1.CreateTaskPushConfig(t.Context(), &a2a.PushConfig{
		TaskID: task.ID,
		URL:    "https://hook.example/callback",
		Token:  "sekret",
	})
	require.NoError(t, err)
	require.NotEmpty(t, saved.ID)

	require.NoError(t, server1.Server.Close())
	require.NoError(t, store1.Close())

	store2, _, client2 := start()
	t.Cleanup(func() { require.NoError(t, store2.Close()) })

	got, err := client2.GetTaskPushConfig(t.Context(), &a2a.GetTaskPushConfigRequest{TaskID: task.ID, ID: saved.ID})
	require.NoError(t, err, "pre-restart task must still be visible to the restarted server")
	assert.Equal(t, "https://hook.example/callback", got.URL)
	assert.Equal(t, "sekret", got.Token)

	restored, err := client2.GetTask(t.Context(), &a2a.GetTaskRequest{ID: task.ID})
	require.NoError(t, err)
	assert.Equal(t, task.ContextID, restored.ContextID)
	assert.Equal(t, a2a.TaskStateCompleted, restored.Status.State)
}

// liveTask is a task left working on a server backed by an a2astore.Store.
type liveTask struct {
	task     *a2a.Task
	server   *echo.Echo
	endpoint string
	store    *a2astore.Store
	received chan a2a.Event
	unblock  func()
}

// startLiveTask opens the store in dir, serves it and leaves a task working
// behind a blocking tool, with a webhook registered for it.
func startLiveTask(t *testing.T, dir string) *liveTask {
	t.Helper()

	received := make(chan a2a.Event, 16)
	webhook := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		var sr a2a.StreamResponse
		if assert.NoError(t, json.NewDecoder(r.Body).Decode(&sr)) {
			received <- sr.Event
		}
	}))
	t.Cleanup(webhook.Close)

	running := make(chan struct{})
	unblock := make(chan struct{})
	release := sync.OnceFunc(func() { close(unblock) })
	t.Cleanup(release)
	tool := tools.Tool{
		Name:       "long_job",
		Parameters: map[string]any{},
		Handler: func(ctx context.Context, _ tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
			close(running)
			select {
			case <-unblock:
			case <-ctx.Done():
			}
			return tools.ResultSuccess("done"), nil
		},
	}
	root := dagent.New("root", "You are a test agent", dagent.WithModel(&sequentialMockProvider{
		id:      modelsdev.NewID("test", "mock-model"),
		streams: []chat.MessageStream{toolCallStream(tool.Name), stopStream()},
	}), dagent.WithTools(tool))

	store, err := a2astore.New(t.Context(), dir)
	require.NoError(t, err)
	deps := newTestTaskDeps(t)
	deps.tasks = store.Tasks(a2asrv.NewTaskStoreAuthenticator())
	deps.pushConfigs = store
	server, endpoint := serveTeam(t, team.New(team.WithAgents(root)), deps)
	result, err := newA2AClient(t, endpoint).SendMessage(t.Context(), &a2a.SendMessageRequest{
		Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("run the long job")),
		Config: &a2a.SendMessageConfig{
			ReturnImmediately: true,
			PushConfig:        &a2a.PushConfig{URL: webhook.URL, Token: "tok"},
		},
	})
	require.NoError(t, err)
	task, ok := result.(*a2a.Task)
	require.True(t, ok)
	select {
	case <-running:
	case <-time.After(10 * time.Second):
		t.Fatal("tool never started")
	}
	return &liveTask{task: task, server: server, endpoint: endpoint, store: store, received: received, unblock: release}
}

// waitPushState waits for the webhook to receive state.
func waitPushState(t *testing.T, received <-chan a2a.Event, state a2a.TaskState) {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev := <-received:
			if pushState(ev) == state {
				return
			}
		case <-timeout:
			t.Fatalf("webhook never received %s", state)
		}
	}
}

// A task still running when the server dies must not stay working forever:
// the restarted server fails it and tells the webhook registered for it.
func TestServer_InFlightTaskFailedAfterRestart(t *testing.T) {
	t.Parallel()

	dir := a2astore.DirFor(filepath.Join(t.TempDir(), "session.db"))
	live := startLiveTask(t, dir)
	// The process dies: the OS releases its store lock.
	require.NoError(t, live.server.Server.Close())
	require.NoError(t, live.store.Close())

	store2, err := a2astore.New(t.Context(), dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store2.Close()) })
	deps2 := newTestTaskDeps(t)
	deps2.tasks = store2.Tasks(a2asrv.NewTaskStoreAuthenticator())
	deps2.pushConfigs = store2
	require.NoError(t, recoverInterruptedTasks(t.Context(), store2, deps2.pushSender))
	_, endpoint2 := serveA2A(t, deps2)

	got, err := newA2AClient(t, endpoint2).GetTask(t.Context(), &a2a.GetTaskRequest{ID: live.task.ID})
	require.NoError(t, err)
	assert.Equal(t, a2a.TaskStateFailed, got.Status.State)
	waitPushState(t, live.received, a2a.TaskStateFailed)
}

// A second server on the same session DB must not recover, and so fail, the
// tasks a live server is running.
func TestRun_SecondServerOnSameSessionDBLeavesLiveTasks(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "DUMMY")

	sessionDB := filepath.Join(t.TempDir(), "session.db")
	live := startLiveTask(t, a2astore.DirFor(sessionDB))
	t.Cleanup(func() { require.NoError(t, live.store.Close()) })

	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, "testdata/basic.yaml", "root", sessionDB, &config.RuntimeConfig{}, ln, RunOptions{PushAllowPrivateNetworks: true})
	}()
	select {
	case err := <-done:
		require.ErrorIs(t, err, a2astore.ErrInUse)
	case <-time.After(10 * time.Second):
		cancel()
		require.NoError(t, <-done)
		t.Fatal("second server started on a store in use")
	}

	got, err := newA2AClient(t, live.endpoint).GetTask(t.Context(), &a2a.GetTaskRequest{ID: live.task.ID})
	require.NoError(t, err)
	assert.Equal(t, a2a.TaskStateWorking, got.Status.State)

	live.unblock()
	waitPushState(t, live.received, a2a.TaskStateCompleted)
	got, err = newA2AClient(t, live.endpoint).GetTask(t.Context(), &a2a.GetTaskRequest{ID: live.task.ID})
	require.NoError(t, err)
	assert.Equal(t, a2a.TaskStateCompleted, got.Status.State)
}

func TestServer_TaskStoreScopedToSessionDB(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	start := func(sessionDB string) *a2aclient.Client {
		store, err := a2astore.New(t.Context(), a2astore.DirFor(sessionDB))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, store.Close()) })
		deps := newTestTaskDeps(t)
		deps.tasks = store.Tasks(a2asrv.NewTaskStoreAuthenticator())
		deps.pushConfigs = store
		_, endpoint := serveA2A(t, deps)
		return newA2AClient(t, endpoint)
	}
	clientA := start(filepath.Join(dir, "a.db"))
	clientB := start(filepath.Join(dir, "b.db"))

	result, err := clientA.SendMessage(t.Context(), &a2a.SendMessageRequest{
		Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hello")),
	})
	require.NoError(t, err)
	task, ok := result.(*a2a.Task)
	require.True(t, ok)
	saved, err := clientA.CreateTaskPushConfig(t.Context(), &a2a.PushConfig{
		TaskID: task.ID, URL: "https://hook.example/cb", Token: "sekret",
	})
	require.NoError(t, err)

	_, err = clientB.GetTask(t.Context(), &a2a.GetTaskRequest{ID: task.ID})
	require.ErrorIs(t, err, a2a.ErrTaskNotFound)
	_, err = clientB.GetTaskPushConfig(t.Context(), &a2a.GetTaskPushConfigRequest{TaskID: task.ID, ID: saved.ID})
	require.ErrorIs(t, err, a2a.ErrTaskNotFound)
}

func startInvokeServer(t *testing.T, tm *team.Team, store session.Store, safety servesafety.Resolved) net.Listener {
	t.Helper()

	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	e, err := newServer(t.Context(), tm, "test.yaml", "root", store, newTestTaskDeps(t), safety, testWorkspaceRoot, ln.Addr().String(), RunOptions{})
	require.NoError(t, err)
	go func() { _ = e.Server.Serve(ln) }()
	t.Cleanup(func() { require.NoError(t, e.Server.Close()) })
	return ln
}
