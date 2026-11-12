package evaluation

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/structuredoutput"
)

func outputEvents(id, agentName, response string, failed bool) []map[string]any {
	return []map[string]any{
		{"type": "tool_call", "agent_name": agentName, "tool_call": map[string]any{
			"id": id, "function": map[string]any{"name": structuredoutput.ToolName, "arguments": response},
		}},
		{
			"type": "tool_call_response", "agent_name": agentName, "tool_call_id": id,
			"tool_definition": map[string]any{"name": structuredoutput.ToolName},
			"response":        response, "result": map[string]any{"isError": failed},
		},
	}
}

func TestEvalStructuredOutputUsesAcceptedFinalAnswer(t *testing.T) {
	t.Parallel()
	events := []map[string]any{{"type": "agent_choice", "agent_name": "root", "content": "Checking the team."}}
	events = append(events, outputEvents("rejected", "root", `{"status":"ready"}`, true)...)
	events = append(events, outputEvents("specialist", "reviewer", `{"status":"ready"}`, false)...)
	events = append(events, outputEvents("accepted", "root", `{"status":"needs_review"}`, false)...)
	events = append(events, map[string]any{"type": "message_added"}, map[string]any{"type": "stream_stopped"})

	response, cost, tokens, calls := parseContainerEvents(events)
	assert.Zero(t, cost)
	assert.Zero(t, tokens)
	assert.Len(t, calls, 3)
	assert.JSONEq(t, `{"status":"needs_review"}`, response)
	results := runAssertions([]session.Assertion{{Name: "final decision", Type: "equals", Value: `{"status":"needs_review"}`}}, response, 0, nil)
	require.Len(t, results, 1)
	assert.True(t, results[0].Passed)

	sess := SessionFromEvents(events, "structured", []string{"Review this team."})
	assert.Equal(t, response, sess.GetLastAssistantMessageContent())
	messages := sess.GetAllMessages()
	last := messages[len(messages)-1]
	assert.Equal(t, "root", last.AgentName)
	assert.Empty(t, last.Message.ToolCalls)
	count := 0
	for _, msg := range messages {
		if msg.Message.Role == "assistant" && msg.Message.Content == response {
			count++
		}
		if msg.Message.ToolCallID == "rejected" {
			assert.True(t, msg.Message.IsError)
		}
	}
	assert.Equal(t, 1, count)
	assert.Contains(t, buildTranscript(events), "[Agent root final answer]:\n"+response)
	data, err := json.Marshal(sess)
	require.NoError(t, err)
	var restored session.Session
	require.NoError(t, json.Unmarshal(data, &restored))
	assert.Equal(t, response, restored.GetLastAssistantMessageContent())
	for name, roundTrip := range replayRoundTrips(t, sess) {
		assert.Equal(t, response, roundTrip.GetLastAssistantMessageContent(), name)
	}
}

func TestEvalStructuredOutputRejectsUntrustedResponses(t *testing.T) {
	t.Parallel()
	for _, mutate := range []struct {
		name string
		edit func([]map[string]any) []map[string]any
	}{
		{"uncorrelated", func(e []map[string]any) []map[string]any { return e[1:] }},
		{"wrong call ID", func(e []map[string]any) []map[string]any { e[1]["tool_call_id"] = "another"; return e }},
		{"wrong agent", func(e []map[string]any) []map[string]any { e[1]["agent_name"] = "other"; return e }},
		{"ordinary tool", func(e []map[string]any) []map[string]any {
			e[0]["tool_call"].(map[string]any)["function"].(map[string]any)["name"] = "get_rules"
			return e
		}},
		{"missing result", func(e []map[string]any) []map[string]any { delete(e[1], "result"); return e }},
		{"rejected", func(e []map[string]any) []map[string]any { e[1]["result"] = map[string]any{"isError": true}; return e }},
		{"malformed", func(e []map[string]any) []map[string]any { e[1]["response"] = "invalid"; return e }},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			t.Parallel()
			events := mutate.edit(outputEvents("output", "root", `{"answer":"ok"}`, false))
			assert.Empty(t, acceptedStructuredOutputs(events))
			response, _, _, _ := parseContainerEvents(events)
			assert.Empty(t, response)
			assert.Empty(t, SessionFromEvents(events, "invalid", nil).GetLastAssistantMessageContent())
		})
	}
}

func TestEvalStructuredOutputDoesNotDuplicateResponses(t *testing.T) {
	t.Parallel()
	events := outputEvents("output", "root", `{"answer":"ok"}`, false)
	events = append(events, events[1])
	assert.Len(t, acceptedStructuredOutputs(events), 1)
}

func TestEvalStructuredOutputFinalTurn(t *testing.T) {
	t.Parallel()
	for _, agent := range []string{"root", "analyzer", ""} {
		t.Run(agent, func(t *testing.T) {
			t.Parallel()
			events := outputEvents("output", agent, `{"answer":"ok"}`, false)
			response, _, _, _ := parseContainerEvents(events)
			assert.JSONEq(t, `{"answer":"ok"}`, response)
			events = append(events, map[string]any{"type": "stream_stopped"},
				map[string]any{"type": "agent_choice", "agent_name": "summary", "content": "The final "},
				map[string]any{"type": "agent_choice", "agent_name": "summary", "content": "summary."})
			response, _, _, _ = parseContainerEvents(events)
			assert.Equal(t, "The final summary.", response)
		})
	}
}

func TestEvalStructuredOutputAttributionAndCorrelation(t *testing.T) {
	t.Parallel()
	events := outputEvents("output", "root", `{"answer":"root"}`, false)
	wrong := outputEvents("output", "other", `{"answer":"wrong"}`, false)[1]
	events = append(events[:1], wrong, events[1])
	assert.Len(t, acceptedStructuredOutputs(events), 1)
	events = outputEvents("output", "", `{"answer":"ok"}`, false)
	delete(events[0], "agent_name")
	delete(events[1], "agent_name")
	assert.Len(t, acceptedStructuredOutputs(events), 1)
	events = append(outputEvents("shared", "root", `{"answer":"root"}`, false), outputEvents("shared", "reader", `{"answer":"reader"}`, false)...)
	assert.Len(t, acceptedStructuredOutputs(events), 2)
}

func TestEvalStructuredOutputFromRuntime(t *testing.T) {
	t.Parallel()
	provider := &replayProvider{streams: [][]chat.MessageStreamResponse{{{
		Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{ToolCalls: []tools.ToolCall{{
			ID: "accepted", Type: "function", Function: tools.FunctionCall{Name: structuredoutput.ToolName, Arguments: `{"answer":"ok"}`},
		}}}, FinishReason: chat.FinishReasonToolCalls}},
		Usage: &chat.Usage{InputTokens: 100, OutputTokens: 50},
	}}}}
	root := agent.New("root", "test", agent.WithModel(provider), agent.WithStructuredOutput(&latest.StructuredOutput{
		Mode:   latest.StructuredOutputModeTool,
		Schema: map[string]any{"type": "object", "properties": map[string]any{"answer": map[string]any{"type": "string"}}, "required": []string{"answer"}},
	}))
	store := modelsdev.NewDatabaseStore(&modelsdev.Database{Providers: map[string]modelsdev.Provider{
		"test": {Models: map[string]modelsdev.Model{"model": {Cost: &modelsdev.Cost{Input: 100}, Limit: modelsdev.Limit{Context: 100_000}, ToolCall: true}}},
	}})
	rt, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), runtime.WithSessionCompaction(false), runtime.WithModelStore(store))
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close() })
	live := session.New(session.WithUserMessage("Answer me"), session.WithNonInteractive(true))
	var events []map[string]any
	for event := range rt.RunStream(t.Context(), live) {
		if event, ok := event.(*runtime.ErrorEvent); ok {
			t.Errorf("runtime error: %s", event.Error)
		}
		data, err := json.Marshal(event)
		require.NoError(t, err)
		var payload map[string]any
		require.NoError(t, json.Unmarshal(data, &payload))
		events = append(events, payload)
	}
	response, cost, tokens, calls := parseContainerEvents(events)
	assert.JSONEq(t, `{"answer":"ok"}`, response)
	assert.Positive(t, tokens)
	assert.InDelta(t, live.TotalCost(), cost, 1e-12)
	assert.Equal(t, []string{structuredoutput.ToolName}, calls)
	for name, restored := range replayRoundTrips(t, SessionFromEvents(events, "runtime", []string{"Answer me"})) {
		t.Run(name, func(t *testing.T) {
			assert.JSONEq(t, response, restored.GetLastAssistantMessageContent())
			assert.InDelta(t, live.TotalCost(), restored.TotalCost(), 1e-12)
			assertToolReplay(t, restored, map[string]string{"accepted": response})
		})
	}
}

func TestEvalStructuredOutputSupportsAllJSONSchemaTypes(t *testing.T) {
	t.Parallel()
	for _, value := range []string{`["one","two"]`, `"answer"`, "42", "true", "null"} {
		t.Run(value, func(t *testing.T) {
			t.Parallel()
			events := outputEvents("output", "root", value, false)
			response, cost, tokens, calls := parseContainerEvents(events)
			assert.Equal(t, value, response)
			assert.Zero(t, cost)
			assert.Zero(t, tokens)
			assert.Len(t, calls, 1)
			assert.Equal(t, value, SessionFromEvents(events, "value", nil).GetLastAssistantMessageContent())
		})
	}
}
