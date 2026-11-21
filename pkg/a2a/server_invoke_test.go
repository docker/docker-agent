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

	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	e, err := newServer(team.New(team.WithAgents(root)), "test.yaml", "root",
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

	dbPath := filepath.Join(t.TempDir(), "a2a.db")
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

func startInvokeServer(t *testing.T, tm *team.Team, store session.Store, safety servesafety.Resolved) net.Listener {
	t.Helper()

	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	e, err := newServer(tm, "test.yaml", "root", store, newTestTaskDeps(t), safety, testWorkspaceRoot, ln.Addr().String(), RunOptions{})
	require.NoError(t, err)
	go func() { _ = e.Server.Serve(ln) }()
	t.Cleanup(func() { require.NoError(t, e.Server.Close()) })
	return ln
}
