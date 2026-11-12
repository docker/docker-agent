package runtime

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	agenttool "github.com/docker/docker-agent/pkg/tools/builtin/agent"
)

func TestEmptyResponseRetainsUsage(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name      string
		content   string
		reasoning string
		finish    chat.FinishReason
		unpriced  bool
	}{
		{name: "empty", finish: chat.FinishReasonStop},
		{name: "whitespace", content: " \n\t", finish: chat.FinishReasonStop},
		{name: "reasoning only", reasoning: "Still thinking", finish: chat.FinishReasonLength},
		{name: "refusal", finish: chat.FinishReasonRefusal},
		{name: "unpriced", finish: chat.FinishReasonStop, unpriced: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			usage := &chat.Usage{InputTokens: 100, OutputTokens: 50, CachedInputTokens: 20, CacheWriteTokens: 10, ReasoningTokens: 40}
			stream := newStreamBuilder().AddContent(tt.content).AddReasoning(tt.reasoning).Build()
			stream.responses = append(stream.responses, chat.MessageStreamResponse{
				Choices: []chat.MessageStreamChoice{{FinishReason: tt.finish}},
				Usage:   usage,
			})
			root := agent.New("root", "test",
				agent.WithModel(&mockProvider{id: "test/mock-model", stream: stream}),
				agent.WithHooks(&latest.HooksConfig{AfterLLMCall: []latest.HookDefinition{{Type: "builtin", Command: "capture-empty-cost"}}}),
			)
			dbPath := filepath.Join(t.TempDir(), "sessions.db")
			store, err := sqlitestore.New(t.Context(), dbPath)
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, store.Close()) })
			var modelStore ModelStore = mockModelStoreWithCost{cost: modelsdev.Cost{Input: 2, Output: 4, CacheRead: 1, CacheWrite: 5}}
			wantCost := 0.00047
			if tt.unpriced {
				modelStore = mockModelStore{}
				wantCost = 0
			}
			rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)),
				WithSessionCompaction(false), WithSessionStore(store), WithModelStore(modelStore),
				WithBudget(&latest.BudgetConfig{MaxCost: 1}),
			)
			require.NoError(t, err)
			var hookInput *hooks.Input
			require.NoError(t, rt.hooksRegistry.RegisterBuiltin("capture-empty-cost",
				func(_ context.Context, in *hooks.Input, _ []string) (*hooks.Output, error) {
					snapshot := *in
					hookInput = &snapshot
					return nil, nil
				}))

			sess := session.New(session.WithUserMessage("hi"), session.WithTitle("Empty response accounting"))
			var message *session.Message
			var lastUsage *Usage
			for event := range rt.RunStream(t.Context(), sess) {
				switch event := event.(type) {
				case *ErrorEvent:
					t.Fatalf("runtime error: %s", event.Error)
				case *MessageAddedEvent:
					message = event.Message
				case *TokenUsageEvent:
					lastUsage = event.Usage
				}
			}
			require.NotNil(t, message)
			assert.Equal(t, "root", message.AgentName)
			assert.Equal(t, "test/mock-model", message.Message.Model)
			assert.Empty(t, message.Message.Content)
			assert.Equal(t, tt.reasoning, message.Message.ReasoningContent)
			assert.Equal(t, tt.finish, message.Message.FinishReason)
			assert.Equal(t, usage, message.Message.Usage)
			assert.InDelta(t, wantCost, message.Message.Cost, 1e-9)
			require.NotNil(t, lastUsage)
			require.NotNil(t, lastUsage.LastMessage)
			assert.Equal(t, *usage, lastUsage.LastMessage.Usage)
			assert.Equal(t, message.Message.Model, lastUsage.LastMessage.Model)
			assert.Equal(t, tt.finish, lastUsage.LastMessage.FinishReason)
			assert.InDelta(t, wantCost, lastUsage.LastMessage.Cost, 1e-9)
			assert.InDelta(t, wantCost, lastUsage.Cost, 1e-9)
			assert.InDelta(t, wantCost, sess.TotalCost(), 1e-9)
			assert.InDelta(t, wantCost, rt.currentBudget().trackers[runBudgetName].snapshot().Cost, 1e-9)
			require.NotNil(t, hookInput)
			if tt.unpriced {
				assert.Nil(t, hookInput.Cost)
			} else {
				require.NotNil(t, hookInput.Cost)
				assert.InDelta(t, wantCost, *hookInput.Cost, 1e-9)
			}

			reopened, err := sqlitestore.New(t.Context(), dbPath)
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, reopened.Close()) })
			loaded, err := reopened.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			for _, s := range []*session.Session{sess, loaded} {
				messages := s.OwnMessages()
				require.Len(t, messages, 2, "exactly one assistant row per turn")
				assert.Equal(t, message.Message, messages[1].Message)
				assert.Equal(t, message.AgentName, messages[1].AgentName)
				assert.InDelta(t, wantCost, s.TotalCost(), 1e-9)
				for _, prompt := range s.GetMessages(root) {
					assert.NotEqual(t, chat.MessageRoleAssistant, prompt.Role, "empty responses must never be replayed to providers")
				}
			}
			summaries, err := reopened.GetSessionSummaries(t.Context())
			require.NoError(t, err)
			require.Len(t, summaries, 1)
			assert.InDelta(t, wantCost, summaries[0].Cost, 1e-9)
		})
	}
}

func TestEmptyResponseWithoutUsageIsSkipped(t *testing.T) {
	t.Parallel()

	sess := session.New(session.WithUserMessage("hi"))
	events := runSession(t, sess, &mockStream{})
	assert.Len(t, sess.OwnMessages(), 1)
	for _, event := range events {
		assert.IsNotType(t, &MessageAddedEvent{}, event)
		if event, ok := event.(*TokenUsageEvent); ok {
			assert.Nil(t, event.Usage.LastMessage)
			assert.True(t, event.AssistantMessageEmpty, "empty completion is visible without token usage")
		}
	}
}

func TestDelegationRetainsOutputAfterBilledEmptyStop(t *testing.T) {
	t.Parallel()

	for _, background := range []bool{false, true} {
		name := "forwarding"
		if background {
			name = "collecting"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			prov := &queueProvider{id: "test/mock-model", streams: []chat.MessageStream{
				newStreamBuilder().AddContent("task result").AddToolCallName("c1", "done").AddToolCallArguments("c1", "{}").AddToolCallStopWithUsage(100, 50).Build(),
				newStreamBuilder().AddStopWithUsage(100, 50).Build(),
			}}
			toolRan := false
			done := tools.Tool{Name: "done", Parameters: map[string]any{}, Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
				toolRan = true
				return tools.ResultSuccess("done"), nil
			}}
			worker := agent.New("worker", "test", agent.WithModel(prov), agent.WithToolSets(newStubToolSet(nil, []tools.Tool{done}, nil)))
			root := agent.New("root", "test", agent.WithModel(prov), agent.WithSubAgents(worker))
			rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root, worker)),
				WithSessionCompaction(false), WithModelStore(mockModelStoreWithCost{cost: modelsdev.Cost{Input: 10, Output: 20}}))
			require.NoError(t, err)
			parent := session.New(session.WithUserMessage("do the task"), session.WithToolsApproved(true))
			require.NoError(t, rt.SessionStore().UpdateSession(t.Context(), parent))
			if background {
				result := rt.RunAgent(t.Context(), agenttool.RunParams{AgentName: "worker", Task: "do the task", ParentSession: parent})
				require.Empty(t, result.ErrMsg)
				assert.Equal(t, "task result", result.Result)
			} else {
				result, err := rt.runForwarding(t.Context(), parent, rt.resolveSessionAgent(parent), EventSinkFunc(func(Event) {}), delegationRequest{
					SubSessionConfig:   SubSessionConfig{AgentName: "worker", Task: "do the task", ToolsApproved: true},
					SwitchCurrentAgent: true,
				})
				require.NoError(t, err)
				assert.Equal(t, "task result", result.Output)
			}
			require.True(t, toolRan)
			child := lastAttachedSubSession(parent)
			require.NotNil(t, child)
			messages := child.OwnMessages()
			require.NotEmpty(t, messages)
			assert.Empty(t, messages[len(messages)-1].Message.Content)
			require.NotNil(t, messages[len(messages)-1].Message.Usage)
			assert.InDelta(t, 0.004, child.TotalCost(), 1e-9, "both the tool-call turn and empty stop must be charged")
			assert.InDelta(t, child.TotalCost(), parent.TotalCost(), 1e-9)
		})
	}
}
