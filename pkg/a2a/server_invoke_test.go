package a2a

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2aclient"
	"github.com/stretchr/testify/require"

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

			client, err := a2aclient.NewFromEndpoints(t.Context(), []a2a.AgentInterface{{
				Transport: a2a.TransportProtocolJSONRPC,
				URL:       fmt.Sprintf("http://%s/invoke", server.Addr()),
			}})
			require.NoError(t, err)
			_, err = client.SendMessage(t.Context(), &a2a.MessageSendParams{
				Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "run the tool"}),
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

	client, err := a2aclient.NewFromEndpoints(t.Context(), []a2a.AgentInterface{{
		Transport: a2a.TransportProtocolJSONRPC,
		URL:       fmt.Sprintf("http://%s/invoke", server.Addr()),
	}})
	require.NoError(t, err)
	message := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "run the tool"})
	message.ContextID = "colliding-context"
	got, err := client.SendMessage(t.Context(), &a2a.MessageSendParams{Message: message})
	require.NoError(t, err)
	task, ok := got.(*a2a.Task)
	require.True(t, ok)
	require.Equal(t, a2a.TaskStateFailed, task.Status.State)
	require.NotNil(t, task.Status.Message)
	require.Len(t, task.Status.Message.Parts, 1)
	failure, ok := task.Status.Message.Parts[0].(a2a.TextPart)
	require.True(t, ok)
	require.Equal(t, "agent run failed: context ID is not available", failure.Text)
	require.Zero(t, executions.Load())

	stored, err := store.GetSession(t.Context(), existing.ID)
	require.NoError(t, err)
	require.Equal(t, "run", stored.Origin)
	require.Equal(t, "private", stored.Title)
}

func startInvokeServer(t *testing.T, tm *team.Team, store session.Store, safety servesafety.Resolved) net.Listener {
	t.Helper()

	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	e, err := newServer(tm, "test.yaml", "root", store, safety, ln.Addr().String(), RunOptions{})
	require.NoError(t, err)
	go func() { _ = e.Server.Serve(ln) }()
	t.Cleanup(func() { require.NoError(t, e.Server.Close()) })
	return ln
}
