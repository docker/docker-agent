package evaluation

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/evaluator"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestSessionFromEventsParallelToolReplay(t *testing.T) {
	t.Parallel()

	for _, order := range [][]string{
		{"evaluation-A", "call-A", "evaluation-B", "call-B", "result-B", "result-A"},
		{"evaluation-A", "call-A", "result-A", "evaluation-B", "call-B", "result-B"},
		{"call-A", "result-A", "call-B", "result-B"}, // No guards: the first result is not a batch boundary either.
	} {
		for _, boundary := range []string{"content", "reasoning", "usage only", "tool only", "stream stopped", "end of input", "error", "budget"} {
			t.Run(order[0]+"/"+order[2]+"/"+boundary, func(t *testing.T) {
				t.Parallel()
				events := []map[string]any{
					{"type": "user_message"},
					{"type": "agent_choice", "content": "Checking.", "agent_name": "root"},
					replayUsageEvent(0.01, 0.01),
				}
				var wantOrder []string
				wantCost := 0.01
				for _, key := range order {
					switch key {
					case "evaluation-A", "evaluation-B":
						event := map[string]any{"type": "evaluation_usage", "evaluation": map[string]any{"id": key, "cost": 0.25}}
						events = append(events, event, event)
						wantOrder = append(wantOrder, key)
						wantCost += 0.25
					case "call-A", "call-B":
						events = append(events, replayToolEvent(key[5:]))
					case "result-A", "result-B":
						events = append(events, map[string]any{"type": "tool_call_response", "tool_call_id": key[7:], "response": key})
						wantOrder = append(wantOrder, key)
					}
				}
				switch boundary {
				case "content", "reasoning", "usage only", "tool only":
					switch boundary {
					case "content":
						events = append(events, map[string]any{"type": "agent_choice", "content": "Done."})
					case "reasoning":
						events = append(events, map[string]any{"type": "agent_choice_reasoning", "content": "Both succeeded."})
					case "tool only":
						events = append(events, map[string]any{"type": "partial_tool_call"})
					}
					wantCost += 0.02
					events = append(events, replayUsageEvent(0.02, wantCost))
					if boundary == "tool only" {
						events = append(events, replayToolEvent("C"), map[string]any{"type": "tool_call_response", "tool_call_id": "C", "response": "result-C"})
					}
					events = append(events, map[string]any{"type": "stream_stopped"})
				case "stream stopped":
					events = append(events, map[string]any{"type": "stream_stopped"})
				case "error":
					events = append(events, map[string]any{"type": "error", "error": "stopped"})
				case "budget":
					events = append(events, budgetExceededTestEvent(), budgetExceededTestEvent())
				}

				sess := SessionFromEvents(events, "parallel", []string{"Run both tools."})
				for name, restored := range replayRoundTrips(t, sess) {
					t.Run(name, func(t *testing.T) {
						items := restored.MessagesSnapshot()
						require.GreaterOrEqual(t, len(items), 2+len(wantOrder))
						batch := items[1].Message
						require.NotNil(t, batch)
						require.Len(t, batch.Message.ToolCalls, 2)
						assert.Equal(t, "A", batch.Message.ToolCalls[0].ID)
						assert.Equal(t, "B", batch.Message.ToolCalls[1].ID)
						require.Len(t, batch.Message.ToolDefinitions, 2)
						assert.Equal(t, "tool-A", batch.Message.ToolDefinitions[0].Name)
						assert.Equal(t, "tool-B", batch.Message.ToolDefinitions[1].Name)
						assert.Equal(t, "Checking.", batch.Message.Content)
						assert.InDelta(t, 0.01, batch.Message.Cost, 1e-12)
						for i, want := range wantOrder {
							item := items[i+2]
							if item.Evaluation != nil {
								assert.Equal(t, want, item.Evaluation.ID)
							} else {
								require.NotNil(t, item.Message)
								assert.Equal(t, want, item.Message.Message.Content)
							}
						}
						assert.InDelta(t, wantCost, restored.TotalCost(), 1e-12)
						_, _, cost := restored.TokensAndCost()
						assert.InDelta(t, wantCost, cost, 1e-12)
						wantResults := map[string]string{"A": "result-A", "B": "result-B"}
						if boundary == "tool only" {
							wantResults["C"] = "result-C"
						}
						assertToolReplay(t, restored, wantResults)
					})
				}
			})
		}
	}
}

func TestSessionFromEventsEvaluationFlushBoundaries(t *testing.T) {
	t.Parallel()

	for _, usageOnly := range []bool{false, true} {
		for _, boundary := range []string{"end", "stream stopped", "error", "budget", "next assistant", "next usage"} {
			t.Run(boundary+"/usage="+strconv.FormatBool(usageOnly), func(t *testing.T) {
				t.Parallel()
				var events []map[string]any
				wantCost := 0.5
				if usageOnly {
					events = append(events, replayUsageEvent(0.01, 0.01))
					wantCost += 0.01
				}
				first := map[string]any{"type": "evaluation_usage", "evaluation": map[string]any{"id": "first", "cost": 0.25}}
				second := map[string]any{"type": "evaluation_usage", "evaluation": map[string]any{"id": "second", "cost": 0.25}}
				events = append(events, first, map[string]any{"type": "evaluation_usage", "evaluation": "malformed"}, second, first)
				switch boundary {
				case "stream stopped":
					events = append(events, map[string]any{"type": "stream_stopped"})
				case "error":
					events = append(events, map[string]any{"type": "error", "error": "denied"})
				case "budget":
					events = append(events, budgetExceededTestEvent(), budgetExceededTestEvent())
				case "next assistant":
					events = append(events, map[string]any{"type": "agent_choice", "content": "Denied."})
				case "next usage":
					wantCost += 0.02
					events = append(events, replayUsageEvent(0.02, wantCost))
				}
				events = append(events, first) // Duplicate delivery after a flush is also harmless.
				sess := SessionFromEvents(events, "denied", []string{"Run it."})
				items := sess.MessagesSnapshot()
				require.NotNil(t, items[0].Message)
				assert.Equal(t, chat.MessageRoleUser, items[0].Message.Message.Role)
				evaluationIndex := 1
				if usageOnly {
					evaluationIndex++
					require.NotNil(t, items[1].Message.Message.Usage)
					assert.InDelta(t, 0.01, items[1].Message.Message.Cost, 1e-12)
				}
				require.NotNil(t, items[evaluationIndex].Evaluation)
				require.NotNil(t, items[evaluationIndex+1].Evaluation)
				assert.Equal(t, "first", items[evaluationIndex].Evaluation.ID)
				assert.Equal(t, "second", items[evaluationIndex+1].Evaluation.ID)
				assert.InDelta(t, wantCost, sess.TotalCost(), 1e-12)
				_, _, cost := sess.TokensAndCost()
				assert.InDelta(t, wantCost, cost, 1e-12)
			})
		}
	}
}

func replayUsageEvent(cost, total float64) map[string]any {
	return map[string]any{"type": "token_usage", "agent_name": "root", "usage": map[string]any{
		"cost": total, "input_tokens": float64(100), "output_tokens": float64(50),
		"last_message": map[string]any{"input_tokens": float64(100), "output_tokens": float64(50), "Cost": cost, "Model": "test/model"},
	}}
}

func replayToolEvent(id string) map[string]any {
	return map[string]any{"type": "tool_call", "tool_call": map[string]any{
		"id": id, "type": "function", "function": map[string]any{"name": "tool-" + id, "arguments": `{}`},
	}, "tool_definition": map[string]any{"name": "tool-" + id}}
}

func replayRoundTrips(t *testing.T, sess *session.Session) map[string]*session.Session {
	t.Helper()
	run := &EvalRun{Name: "replay", Results: []Result{{Session: sess}}}
	path, err := SaveRunSessionsJSON(run, t.TempDir())
	require.NoError(t, err)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var exported RunOutput
	require.NoError(t, json.Unmarshal(data, &exported))
	require.Len(t, exported.Sessions, 1)

	path, err = SaveRunSessions(t.Context(), run, t.TempDir())
	require.NoError(t, err)
	store, err := sqlitestore.New(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	loaded, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	return map[string]*session.Session{"original": sess, "JSON": exported.Sessions[0], "SQLite": loaded}
}

func assertToolReplay(t *testing.T, sess *session.Session, want map[string]string) {
	t.Helper()
	var raw []chat.Message
	for _, item := range sess.MessagesSnapshot() {
		if item.Message != nil && item.Message.Message.Role != chat.MessageRoleSystem {
			raw = append(raw, item.Message.Message)
		}
	}
	for name, messages := range map[string][]chat.Message{"raw": raw, "provider": sess.GetMessages(&agent.Agent{})} {
		pending := make(map[string]bool)
		results := make(map[string]string)
		for _, msg := range messages {
			switch msg.Role {
			case chat.MessageRoleAssistant, chat.MessageRoleUser:
				assert.Empty(t, pending, "%s: a new turn interrupted a tool batch", name)
				for _, call := range msg.ToolCalls {
					pending[call.ID] = true
				}
			case chat.MessageRoleTool:
				assert.True(t, pending[msg.ToolCallID], "%s: unexpected result for %s", name, msg.ToolCallID)
				delete(pending, msg.ToolCallID)
				assert.False(t, msg.IsError, "%s: replay synthesized an error", name)
				results[msg.ToolCallID] = msg.Content
			}
		}
		assert.Empty(t, pending, name)
		assert.Equal(t, want, results, name)
	}
}

func TestSessionFromEventsRuntimeParallelGuards(t *testing.T) {
	t.Parallel()

	for _, earlyResult := range []bool{false, true} {
		name := "results B then A"
		if earlyResult {
			name = "result A before guard B"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			releaseGuardB := make(chan struct{})
			releaseToolA := make(chan struct{})
			guard := replayEvaluator(func(ctx context.Context, state any) (*evaluator.Result, error) {
				name := state.(map[string]any)["tool_name"].(string)
				if name == "tool-B" {
					select {
					case <-releaseGuardB:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
				evaluator.ObserveUsage(ctx, evaluator.UsageRecord{Model: name, Usage: &evaluator.Usage{InputTokens: 10}, Cost: new(0.25)})
				return &evaluator.Result{Type: "boolean", Probability: new(1.0)}, nil
			})
			var toolset replayToolSet
			var calls []tools.ToolCall
			for _, id := range []string{"A", "B"} {
				calls = append(calls, tools.ToolCall{ID: id, Type: "function", Function: tools.FunctionCall{Name: "tool-" + id, Arguments: `{}`}})
				toolset = append(toolset, tools.Tool{Name: "tool-" + id, Handler: func(ctx context.Context, call tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
					if call.ID == "A" && !earlyResult {
						select {
						case <-releaseToolA:
						case <-ctx.Done():
							return nil, ctx.Err()
						}
					}
					return tools.ResultSuccess("result-" + call.ID), nil
				}})
			}
			provider := &replayProvider{streams: [][]chat.MessageStreamResponse{
				{{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{ToolCalls: calls}, FinishReason: chat.FinishReasonToolCalls}}, Usage: &chat.Usage{InputTokens: 100, OutputTokens: 50}}},
				{{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{Content: "Done."}, FinishReason: chat.FinishReasonStop}}, Usage: &chat.Usage{InputTokens: 200, OutputTokens: 25}}},
			}}
			root := agent.New("root", "test", agent.WithModel(provider), agent.WithToolSets(toolset), agent.WithHooks(&latest.HooksConfig{
				ToolGuard: []latest.HookMatcherConfig{{Hooks: []latest.HookDefinition{{
					Type: hooks.HookTypeEvaluator, Evaluator: "safety",
					EvaluatorPolicy: &latest.EvaluatorPolicy{Decisions: map[string]string{"true": "allow"}, MinProbability: 0.9, Fallback: "deny"},
				}}}},
			}))
			tm := team.New(team.WithAgents(root), team.WithEvaluators(map[string]evaluator.Evaluator{"safety": guard}))
			modelStore := modelsdev.NewDatabaseStore(&modelsdev.Database{Providers: map[string]modelsdev.Provider{
				"test": {Models: map[string]modelsdev.Model{"model": {Cost: &modelsdev.Cost{Input: 100}, Limit: modelsdev.Limit{Context: 100_000}, ToolCall: true}}},
			}})
			rt, err := runtime.NewLocalRuntime(ctx, tm, runtime.WithSessionCompaction(false), runtime.WithModelStore(modelStore))
			require.NoError(t, err)
			t.Cleanup(func() { _ = rt.Close() })
			live := session.New(session.WithUserMessage("Run both tools."), session.WithTitle("test"), session.WithSafetyPolicy(session.SafetyPolicyAutonomous), session.WithNonInteractive(true))
			var events []map[string]any
			var order []string
			for event := range rt.RunStream(ctx, live) {
				data, err := json.Marshal(event)
				require.NoError(t, err)
				var payload map[string]any
				require.NoError(t, json.Unmarshal(data, &payload))
				events = append(events, payload)
				switch e := event.(type) {
				case *runtime.EvaluationUsageEvent:
					order = append(order, "evaluation-"+e.Evaluation.Model)
				case *runtime.ToolCallEvent:
					order = append(order, "call-"+e.ToolCall.ID)
					if e.ToolCall.ID == "A" && !earlyResult {
						close(releaseGuardB)
					}
				case *runtime.ToolCallResponseEvent:
					order = append(order, "result-"+e.ToolCallID)
					if e.ToolCallID == "A" && earlyResult {
						close(releaseGuardB)
					}
					if e.ToolCallID == "B" && !earlyResult {
						close(releaseToolA)
					}
				case *runtime.ErrorEvent:
					t.Errorf("unexpected runtime error: %s", e.Error)
				}
			}
			require.NoError(t, ctx.Err())
			wantOrder := []string{"evaluation-tool-A", "call-A", "evaluation-tool-B", "call-B", "result-B", "result-A"}
			if earlyResult {
				wantOrder = []string{"evaluation-tool-A", "call-A", "result-A", "evaluation-tool-B", "call-B", "result-B"}
			}
			require.Equal(t, wantOrder, order)
			assert.InDelta(t, 0.53, live.TotalCost(), 1e-12)
			var wantRecords []*session.Evaluation
			for _, item := range live.MessagesSnapshot() {
				if item.Evaluation != nil {
					wantRecords = append(wantRecords, item.Evaluation)
				}
			}
			require.Len(t, wantRecords, 2)
			sess := SessionFromEvents(events, "runtime", []string{"Run both tools."})
			for name, restored := range replayRoundTrips(t, sess) {
				t.Run(name, func(t *testing.T) {
					assertToolReplay(t, restored, map[string]string{"A": "result-A", "B": "result-B"})
					assert.InDelta(t, live.TotalCost(), restored.TotalCost(), 1e-12)
					_, _, cost := restored.TokensAndCost()
					assert.InDelta(t, live.TotalCost(), cost, 1e-12)
					var records []*session.Evaluation
					for _, item := range restored.MessagesSnapshot() {
						if item.Evaluation != nil {
							records = append(records, item.Evaluation)
						}
					}
					assert.Equal(t, wantRecords, records)
				})
			}
		})
	}
}

type replayEvaluator func(context.Context, any) (*evaluator.Result, error)

func (f replayEvaluator) Evaluate(ctx context.Context, state any) (*evaluator.Result, error) {
	return f(ctx, state)
}

type replayToolSet []tools.Tool

func (ts replayToolSet) Tools(context.Context) ([]tools.Tool, error) { return ts, nil }

type replayProvider struct {
	mu      sync.Mutex
	streams [][]chat.MessageStreamResponse
}

func (*replayProvider) ID() modelsdev.ID        { return modelsdev.NewID("test", "model") }
func (*replayProvider) BaseConfig() base.Config { return base.Config{} }

func (p *replayProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.streams) == 0 {
		return &replayStream{}, nil
	}
	stream := &replayStream{responses: p.streams[0]}
	p.streams = p.streams[1:]
	return stream, nil
}

type replayStream struct {
	responses []chat.MessageStreamResponse
}

func (s *replayStream) Recv() (chat.MessageStreamResponse, error) {
	if len(s.responses) == 0 {
		return chat.MessageStreamResponse{}, io.EOF
	}
	response := s.responses[0]
	s.responses = s.responses[1:]
	return response, nil
}

func (*replayStream) Close() {}

func TestSessionFromEventsRepeatedEvaluationDoesNotSplitNextResponse(t *testing.T) {
	t.Parallel()
	event := map[string]any{"type": "evaluation_usage", "evaluation": map[string]any{"id": "request", "cost": 0.25}}
	sess := SessionFromEvents([]map[string]any{
		{"type": "user_message"},
		replayUsageEvent(0.01, 0.01),
		event,
		replayToolEvent("A"),
		{"type": "tool_call_response", "tool_call_id": "A", "response": "result-A"},
		{"type": "agent_choice", "content": "All"},
		event,
		{"type": "agent_choice", "content": " done."},
		replayUsageEvent(0.02, 0.28),
		{"type": "stream_stopped"},
		event,
		{"type": "user_message"},
		{"type": "agent_choice", "content": "Next answer."},
	}, "repeated", []string{"Run it.", "Next question."})
	items := sess.MessagesSnapshot()
	require.Len(t, items, 7)
	assert.Equal(t, "All done.", items[4].Message.Message.Content)
	assert.Equal(t, "Next question.", items[5].Message.Message.Content)
	assert.Equal(t, "Next answer.", items[6].Message.Message.Content)
	assert.InDelta(t, 0.28, sess.TotalCost(), 1e-12)
	assertToolReplay(t, sess, map[string]string{"A": "result-A"})
}
