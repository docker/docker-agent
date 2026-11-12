package acp

import (
	"strings"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/todo"
)

func TestRunAgentEmptyPlanSnapshots(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		tool     string
		result   *tools.ToolCallResult
		wantPlan bool
	}{
		{name: "empty todos", tool: todo.ToolNameListTodos, result: &tools.ToolCallResult{Meta: []todo.Todo{}}, wantPlan: true},
		{name: "typed nil todos", tool: todo.ToolNameListTodos, result: &tools.ToolCallResult{Meta: []todo.Todo(nil)}, wantPlan: true},
		{name: "failed update with authoritative snapshot", tool: todo.ToolNameUpdateTodos, result: &tools.ToolCallResult{IsError: true, Meta: []todo.Todo(nil)}, wantPlan: true},
		{name: "nil result", tool: todo.ToolNameListTodos},
		{name: "missing metadata", tool: todo.ToolNameListTodos, result: tools.ResultSuccess(`{"todos":[]}`)},
		{name: "unrelated metadata", tool: todo.ToolNameListTodos, result: &tools.ToolCallResult{Meta: []any{}}},
		{name: "non-todo tool", tool: "other", result: &tools.ToolCallResult{Meta: []todo.Todo{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rt := &fakeRuntime{events: []runtime.Event{
				runtime.ToolCallResponse("initial", tools.Tool{Name: todo.ToolNameCreateTodo}, &tools.ToolCallResult{Meta: []todo.Todo{{Description: "existing", Status: "pending"}}}, "initial", "root"),
				runtime.ToolCallResponse("next", tools.Tool{Name: tc.tool}, tc.result, "transformed response", "root"),
				runtime.AgentChoice("root", testSessionID, "continued"),
			}}
			f := newRunAgentFixture(t, rt, &captureWriter{})
			require.NoError(t, f.runAgent(t.Context(), f.sess))
			updates := f.sessionUpdates(t)
			require.NotNil(t, updates[2].Plan)
			require.Len(t, updates[2].Plan.Entries, 1)
			require.NotNil(t, updates[3].ToolCall)
			assert.Equal(t, "transformed response", updates[3].ToolCall.Content[0].Content.Content.Text.Text)
			if tc.wantPlan {
				require.Len(t, updates, 6)
				require.NotNil(t, updates[4].Plan)
				assert.Equal(t, []acpsdk.PlanEntry{}, updates[4].Plan.Entries)
				assert.Contains(t, strings.Join(f.out.lines(), "\n"), `"entries":[]`)
			} else {
				require.Len(t, updates, 5)
				assert.Nil(t, updates[4].Plan)
			}
			assert.Equal(t, "continued", agentMessageText(t, updates[len(updates)-1]))
		})
	}
}

func TestPromptTodoStorageClearsDisplayedPlan(t *testing.T) {
	t.Parallel()
	storage := todo.NewMemoryTodoStorage()
	storage.Add(t.Context(), todo.Todo{ID: "1", Description: "existing", Status: "pending"})
	prov := &outcomeSequenceProvider{mockProvider: mockProvider{id: modelsdev.NewID("test", "plan")}}
	for range 3 {
		prov.streams = append(prov.streams,
			&mockStream{responses: []chat.MessageStreamResponse{{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{ToolCalls: []tools.ToolCall{{ID: "list", Type: "function", Function: tools.FunctionCall{Name: todo.ToolNameListTodos, Arguments: "{}"}}}}, FinishReason: chat.FinishReasonToolCalls}}}}},
			&mockStream{responses: []chat.MessageStreamResponse{
				{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{Content: "Done"}}}},
				{Choices: []chat.MessageStreamChoice{{FinishReason: chat.FinishReasonStop}}},
			}},
		)
	}
	root := agent.New("root", "test", agent.WithModel(prov), agent.WithToolSets(todo.New(todo.WithStorage(storage))))
	rt, err := runtime.New(t.Context(), team.New(team.WithAgents(root)), runtime.WithSessionCompaction(false))
	require.NoError(t, err)
	defer func() { require.NoError(t, rt.Close()) }()
	a, s, peer := newPromptTestAgent(t, rt)
	s.sess.SetToolsApproved(true)
	out := peer.out.(*captureWriter)
	for turn := range 3 {
		if turn == 1 {
			storage.Update(t.Context(), 0, func(item todo.Todo) todo.Todo { item.Status = "completed"; return item })
		}
		if turn == 2 {
			storage.Clear(t.Context())
		}
		response, err := a.Prompt(t.Context(), promptRequest("list tasks"))
		require.NoError(t, err)
		assert.Equal(t, acpsdk.StopReasonEndTurn, response.StopReason)
		var plans []*acpsdk.SessionUpdatePlan
		f := &runAgentFixture{out: out}
		for _, update := range f.sessionUpdates(t) {
			if update.Plan != nil {
				plans = append(plans, update.Plan)
			}
		}
		require.Len(t, plans, turn+1, "each snapshot replaces the displayed plan")
		if turn < 2 {
			require.Len(t, plans[turn].Entries, 1)
			expected := acpsdk.PlanEntryStatusPending
			if turn == 1 {
				expected = acpsdk.PlanEntryStatusCompleted
			}
			assert.Equal(t, expected, plans[turn].Entries[0].Status)
		} else {
			assert.Equal(t, []acpsdk.PlanEntry{}, plans[turn].Entries)
			assert.Contains(t, strings.Join(out.lines(), "\n"), `"entries":[]`)
		}
	}
}
