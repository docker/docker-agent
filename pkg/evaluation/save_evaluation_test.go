package evaluation

import (
	"encoding/json"
	"math"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
)

func TestSessionFromEventsEvaluationUsage(t *testing.T) {
	t.Parallel()

	for _, content := range []string{"", "Checking the tool."} {
		for _, tc := range []struct {
			name string
			cost *float64
		}{
			{name: "unknown"},
			{name: "free", cost: new(float64)},
			{name: "paid", cost: new(0.25)},
			{name: "missing usage"},
		} {
			t.Run(content+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				cost := tc.cost
				evaluation := &session.Evaluation{
					ID: "request-1", Evaluator: "safety", AgentName: "root", Model: "jev-1.13.0",
					Usage: &chat.Usage{InputTokens: 12, OutputTokens: 3}, Cost: cost,
					CreatedAt: "2026-09-22T12:00:00.123Z",
				}
				if tc.name == "missing usage" {
					evaluation.Usage = nil
				}
				data, err := json.Marshal(evaluation)
				require.NoError(t, err)
				var payload map[string]any
				require.NoError(t, json.Unmarshal(data, &payload))
				event := map[string]any{"type": "evaluation_usage", "evaluation": payload}
				wantCost := 0.03
				if cost != nil {
					wantCost += *cost
				}
				events := []map[string]any{
					{"type": "user_message"},
					{"type": "agent_choice", "content": content, "agent_name": "root"},
					{"type": "token_usage", "usage": map[string]any{
						"input_tokens": float64(100), "output_tokens": float64(50), "cost": 0.01,
						"last_message": map[string]any{"input_tokens": float64(100), "output_tokens": float64(50), "Cost": 0.01, "Model": "chat-model"},
					}},
					event,
					event, // Repeated delivery must not bill the request twice.
					{"type": "token_usage", "usage": map[string]any{
						"input_tokens": float64(100), "output_tokens": float64(50), "cost": wantCost - 0.02,
					}},
					{"type": "tool_call", "agent_name": "root", "tool_call": map[string]any{
						"id": "call-1", "type": "function", "function": map[string]any{"name": "shell", "arguments": `{}`},
					}},
					{"type": "tool_call_response", "tool_call_id": "call-1", "response": "done"},
					{"type": "agent_choice", "content": "Finished.", "agent_name": "root"},
					{"type": "token_usage", "usage": map[string]any{
						"input_tokens": float64(200), "output_tokens": float64(25), "cost": wantCost,
						"last_message": map[string]any{"input_tokens": float64(200), "output_tokens": float64(25), "Cost": 0.02, "Model": "chat-model"},
					}},
					{"type": "stream_stopped"},
				}
				sess := SessionFromEvents(events, "accounting", []string{"Run the tool."})
				items := sess.MessagesSnapshot()
				const evaluationIndex = 2
				assert.Equal(t, content, items[1].Message.Message.Content)
				require.Len(t, items, evaluationIndex+3)
				assert.Equal(t, evaluation, items[evaluationIndex].Evaluation)
				toolMessage := items[1].Message
				require.NotNil(t, toolMessage)
				require.Len(t, toolMessage.Message.ToolCalls, 1)
				assert.Equal(t, "call-1", toolMessage.Message.ToolCalls[0].ID)
				usageMessage := toolMessage
				assert.Equal(t, &chat.Usage{InputTokens: 100, OutputTokens: 50}, usageMessage.Message.Usage)
				assert.Equal(t, "chat-model", usageMessage.Message.Model)
				assert.InDelta(t, 0.01, usageMessage.Message.Cost, 1e-12)
				assert.InDelta(t, wantCost, sess.TotalCost(), 1e-12)
				input, output, total := sess.TokensAndCost()
				assert.EqualValues(t, 200, input)
				assert.EqualValues(t, 25, output)
				assert.InDelta(t, wantCost, total, 1e-12)

				run := &EvalRun{Name: "accounting", Results: []Result{{Session: sess}}}
				path, err := SaveRunSessionsJSON(run, t.TempDir())
				require.NoError(t, err)
				data, err = os.ReadFile(path)
				require.NoError(t, err)
				var exported RunOutput
				require.NoError(t, json.Unmarshal(data, &exported))
				require.Len(t, exported.Sessions, 1)
				assert.Equal(t, evaluation, exported.Sessions[0].Messages[evaluationIndex].Evaluation)
				assert.InDelta(t, wantCost, exported.Sessions[0].TotalCost(), 1e-12)

				path, err = SaveRunSessions(t.Context(), run, t.TempDir())
				require.NoError(t, err)
				store, err := sqlitestore.New(t.Context(), path)
				require.NoError(t, err)
				t.Cleanup(func() { _ = store.Close() })
				loaded, err := store.GetSession(t.Context(), sess.ID)
				require.NoError(t, err)
				assert.Equal(t, evaluation, loaded.Messages[evaluationIndex].Evaluation)
				assert.InDelta(t, wantCost, loaded.TotalCost(), 1e-12)
				_, _, total = loaded.TokensAndCost()
				assert.InDelta(t, wantCost, total, 1e-12)
			})
		}
	}
}

func TestSessionFromEventsEvaluationUsageMalformed(t *testing.T) {
	t.Parallel()

	for name, payload := range map[string]any{
		"missing":       nil,
		"wrong type":    "not an evaluation",
		"missing ID":    map[string]any{"cost": 1},
		"invalid cost":  map[string]any{"id": "bad", "cost": "unknown"},
		"unencodable":   map[string]any{"id": "bad", "cost": math.NaN()},
		"invalid usage": map[string]any{"id": "bad", "usage": map[string]any{"input_tokens": "invalid"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sess := SessionFromEvents([]map[string]any{
				{"type": "agent_choice", "content": "Before"},
				{"type": "evaluation_usage", "evaluation": payload},
				{"type": "agent_choice", "content": " after"},
				{"type": "stream_stopped"},
			}, "malformed", []string{"question"})
			require.Len(t, sess.Messages, 2)
			assert.Equal(t, "Before after", sess.Messages[1].Message.Message.Content)
			assert.Zero(t, sess.TotalCost())
		})
	}
}

func TestSessionFromEventsEvaluationDeniesUsageOnlyTurn(t *testing.T) {
	t.Parallel()

	sess := SessionFromEvents([]map[string]any{
		{"type": "user_message"},
		{"type": "token_usage", "agent_name": "root", "timestamp": "2026-09-22T12:00:00Z", "usage": map[string]any{
			"input_tokens": float64(100), "output_tokens": float64(50), "cost": 0.01,
			"last_message": map[string]any{"input_tokens": float64(100), "output_tokens": float64(50), "Cost": 0.01, "Model": "chat-model"},
		}},
		{"type": "evaluation_usage", "evaluation": map[string]any{"id": "denied", "cost": 0.25}},
		{"type": "token_usage", "usage": map[string]any{"input_tokens": float64(100), "output_tokens": float64(50), "cost": 0.26}},
		{"type": "tool_call_response", "tool_call_id": "blocked", "response": "denied by evaluator"},
		{"type": "stream_stopped"},
	}, "denied", []string{"Run the tool."})

	require.Len(t, sess.Messages, 4)
	usage := sess.Messages[1].Message
	require.NotNil(t, usage)
	assert.Empty(t, usage.Message.Content)
	assert.Equal(t, "root", usage.AgentName)
	assert.Equal(t, "2026-09-22T12:00:00Z", usage.Message.CreatedAt)
	assert.Equal(t, &chat.Usage{InputTokens: 100, OutputTokens: 50}, usage.Message.Usage)
	assert.Equal(t, "denied", sess.Messages[2].Evaluation.ID)
	assert.Equal(t, chat.MessageRoleTool, sess.Messages[3].Message.Message.Role)
	assert.InDelta(t, 0.26, sess.TotalCost(), 1e-12)
	_, _, cost := sess.TokensAndCost()
	assert.InDelta(t, 0.26, cost, 1e-12)
}
