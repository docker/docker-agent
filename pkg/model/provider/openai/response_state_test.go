package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/chatgpt"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/tools"
)

func responseStateOutput() []json.RawMessage {
	return []json.RawMessage{
		json.RawMessage(`{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"opaque"}`),
		json.RawMessage(`{"type":"message","id":"msg_1","role":"assistant","status":"completed","phase":"commentary","content":[{"type":"output_text","text":"Checking.","annotations":[]}]}`),
		json.RawMessage(`{"type":"tool_search_call","id":"ts_1","execution":"server","call_id":null,"arguments":{"query":"read"},"status":"completed"}`),
		json.RawMessage(`{"type":"tool_search_output","id":"tso_1","execution":"server","call_id":null,"status":"completed","tools":[{"type":"function","name":"read","parameters":{"type":"object","properties":{},"additionalProperties":false},"strict":true}]}`),
		json.RawMessage(`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read","arguments":"{}","status":"completed"}`),
	}
}

func responseStateMessage() chat.Message {
	return chat.Message{
		Role: chat.MessageRoleAssistant, Content: "Checking.",
		ToolCalls:      []tools.ToolCall{{ID: "call_1", Type: "function", Function: tools.FunctionCall{Name: "read", Arguments: "{}"}}},
		OpenAIResponse: &chat.OpenAIResponse{ID: "resp_1", Source: "openai/gpt-5.6", SessionID: "session_1", Output: responseStateOutput()},
	}
}

func responseStateClient() *Client {
	return &Client{Config: base.Config{ModelConfig: latest.ModelConfig{Provider: "openai", Model: "gpt-5.6", ProviderOpts: map[string]any{"preserve_reasoning": true}}}}
}

func TestResponseStateReplay(t *testing.T) {
	t.Parallel()
	client := responseStateClient()
	message := responseStateMessage()
	items := client.convertMessagesToResponseInput(t.Context(), []chat.Message{message, {
		Role: chat.MessageRoleTool, ToolCallID: "call_1", Content: "ok",
	}})
	require.Len(t, items, 6)
	for i, want := range message.OpenAIResponse.Output {
		got, err := json.Marshal(items[i])
		require.NoError(t, err)
		assert.JSONEq(t, string(want), string(got))
	}
	assert.Equal(t, "call_1", items[5].OfFunctionCallOutput.CallID.Value)
}

func TestResponseStateDoesNotBypassEdits(t *testing.T) {
	t.Parallel()
	for name, edit := range map[string]func(*chat.Message, *Client){
		"text redaction":     func(m *chat.Message, _ *Client) { m.Content = "redacted" },
		"tool argument edit": func(m *chat.Message, _ *Client) { m.ToolCalls[0].Function.Arguments = `{"safe":true}` },
		"tool removal":       func(m *chat.Message, _ *Client) { m.ToolCalls = nil },
		"model switch":       func(_ *chat.Message, c *Client) { c.ModelConfig.Model = "gpt-6-astra" },
		"vendor switch":      func(_ *chat.Message, c *Client) { c.ModelConfig.Provider = "xai" },
		"custom endpoint":    func(_ *chat.Message, c *Client) { c.ModelConfig.BaseURL = "https://example.com/v1" },
		"non assistant":      func(m *chat.Message, _ *Client) { m.Role = chat.MessageRoleUser },
		"invalid item":       func(m *chat.Message, _ *Client) { m.OpenAIResponse.Output[0] = json.RawMessage(`{`) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m, c := responseStateMessage(), responseStateClient()
			edit(&m, c)
			assert.Empty(t, c.replayResponse(m))
		})
	}
}

func TestResponseStateStream(t *testing.T) {
	t.Parallel()
	for _, completedOutput := range []bool{false, true} {
		t.Run(strconv.FormatBool(completedOutput), func(t *testing.T) {
			t.Parallel()
			var events []map[string]any
			for i, item := range responseStateOutput() {
				events = append(events, map[string]any{"type": "response.output_item.done", "output_index": i, "item": item})
			}
			// A repeated done event must not duplicate opaque state.
			events = append(events, events[0])
			response := map[string]any{"id": "resp_1", "output": []any{}}
			if completedOutput {
				response["output"] = responseStateOutput()
			}
			events = append(events, map[string]any{"type": "response.completed", "response": response})
			client := responseStateClient()
			adapter := client.responseAdapter(httpclient.ContextWithSessionID(t.Context(), "session_1"), &fakeEventStream{events: decodeEvents(t, events)})
			var state *chat.OpenAIResponse
			for {
				r, err := adapter.Recv()
				if errors.Is(err, io.EOF) {
					break
				}
				require.NoError(t, err)
				for _, c := range r.Choices {
					if c.Delta.OpenAIResponse != nil {
						state = c.Delta.OpenAIResponse
					}
				}
			}
			require.NotNil(t, state)
			assert.Equal(t, "resp_1", state.ID)
			assert.Equal(t, "session_1", state.SessionID)
			require.Len(t, state.Output, 5)
			for i, want := range responseStateOutput() {
				assert.JSONEq(t, string(want), string(state.Output[i]))
			}
		})
	}
}

func TestCacheDiagnosticsScopedToHistory(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, sessionID, model, provider, baseURL string
		enabled                                   bool
		want                                      string
	}{
		{name: "same session", sessionID: "session_1", enabled: true, want: "resp_1"},
		{name: "other session", sessionID: "session_2", enabled: true},
		{name: "no session", enabled: true},
		{name: "disabled", sessionID: "session_1"},
		{name: "older model", sessionID: "session_1", model: "gpt-5.4", enabled: true},
		{name: "model switch", sessionID: "session_1", model: "gpt-6-astra", enabled: true},
		{name: "chatgpt", sessionID: "session_1", provider: "chatgpt", enabled: true},
		{name: "custom endpoint", sessionID: "session_1", baseURL: "https://example.com", enabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := responseStateClient()
			client.ModelConfig.ProviderOpts = map[string]any{"cache_diagnostics": tc.enabled}
			if tc.model != "" {
				client.ModelConfig.Model = tc.model
			}
			if tc.provider != "" {
				client.ModelConfig.Provider = tc.provider
			}
			client.ModelConfig.BaseURL = tc.baseURL
			var params responses.ResponseNewParams
			client.configureResponseState(httpclient.ContextWithSessionID(t.Context(), tc.sessionID), &params, []chat.Message{responseStateMessage()})
			assert.Equal(t, tc.want, params.PromptCacheOptions.ComparisonResponseID.Value)
		})
	}
}

func TestToolSearchReplayHonorsCurrentCatalog(t *testing.T) {
	t.Parallel()
	client := responseStateClient()
	client.ModelConfig.ProviderOpts = map[string]any{"native_tool_search": true}
	items := client.replayResponse(responseStateMessage())
	require.Len(t, items, 5)
	filtered := client.filterToolSearchHistory(items, nil)
	for _, item := range filtered {
		assert.Nil(t, item.OfToolSearchCall)
		assert.Nil(t, item.OfToolSearchOutput)
	}
	items = client.replayResponse(responseStateMessage())
	assert.Len(t, client.filterToolSearchHistory(items, []tools.Tool{{Name: "read"}}), 5)
	client.ModelConfig.ProviderOpts = map[string]any{"preserve_reasoning": true}
	items = client.replayResponse(responseStateMessage())
	filtered = client.filterToolSearchHistory(items, []tools.Tool{{Name: "read"}})
	assert.Len(t, filtered, 3)
}

func TestResponsesShortlistWire(t *testing.T) {
	t.Parallel()
	requests := make(chan map[string]json.RawMessage, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		requests <- request
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_2\",\"output\":[]}}\n\n")
	}))
	t.Cleanup(server.Close)
	sdk := openai.NewClient(option.WithBaseURL(server.URL), option.WithAPIKey("test"))
	client := responseStateClient()
	client.clientFn = func(context.Context) (*openai.Client, error) { return &sdk, nil }
	client.ModelConfig.ProviderOpts = map[string]any{"cache_diagnostics": true, "native_tool_search": true}
	messages := []chat.Message{responseStateMessage(), {Role: chat.MessageRoleTool, ToolCallID: "call_1", Content: "ok"}}
	requestTools := []tools.Tool{{Name: "read", InCatalog: true, SearchOnly: true, Parameters: map[string]any{"type": "object"}}}
	for _, sessionID := range []string{"session_1", "session_2"} {
		stream, err := client.CreateResponseStream(httpclient.ContextWithSessionID(t.Context(), sessionID), messages, requestTools)
		require.NoError(t, err)
		drainReasoningTestStream(t, stream)
		stream.Close()
		request := <-requests
		if sessionID == "session_1" {
			assert.JSONEq(t, `{"comparison_response_id":"resp_1"}`, string(request["prompt_cache_options"]))
		} else {
			assert.NotContains(t, request, "prompt_cache_options")
		}
		assert.JSONEq(t, `["reasoning.encrypted_content"]`, string(request["include"]))
		assert.NotContains(t, request, "previous_response_id")
		var declarations []hostedToolPayload
		require.NoError(t, json.Unmarshal(request["tools"], &declarations))
		require.Len(t, declarations, 2)
		assert.Equal(t, "read", declarations[0].Name)
		assert.Equal(t, new(true), declarations[0].DeferLoading)
		assert.Equal(t, "tool_search", declarations[1].Type)
		assert.Equal(t, "server", declarations[1].Execution)
		assert.Contains(t, string(request["input"]), `"encrypted_content":"opaque"`)
		assert.Contains(t, string(request["input"]), `"phase":"commentary"`)
	}
}

func TestCacheDiagnosticsLogging(t *testing.T) {
	var log bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	var response responses.Response
	require.NoError(t, json.Unmarshal([]byte(`{"id":"resp_2","prompt_cache_diagnostics":{"type":"cache_miss","reason":"tools_changed","cache_missed_tokens":2048,"comparison_reusable_tokens":4096}}`), &response))
	logCacheDiagnostics(httpclient.ContextWithSessionID(t.Context(), "session_1"), response)
	assert.Contains(t, log.String(), `"reason":"tools_changed"`)
	assert.Contains(t, log.String(), `"cache_missed_tokens":2048`)
	assert.Contains(t, log.String(), `"session_id":"session_1"`)
	assert.NotContains(t, log.String(), "encrypted_content")
}

func TestResponseStateRejectsRedactedReasoning(t *testing.T) {
	t.Parallel()
	message := responseStateMessage()
	message.OpenAIResponse.Output[0] = json.RawMessage(`{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"secret"}],"encrypted_content":"opaque"}`)
	message.ReasoningContent = "secret"
	client := responseStateClient()
	require.Len(t, client.replayResponse(message), 5)
	message.ReasoningContent = "[REDACTED]"
	assert.Nil(t, client.replayResponse(message))
}

func TestResponseStateUsesResolvedModel(t *testing.T) {
	t.Parallel()
	client := responseStateClient()
	client.ModelConfig.DisplayModel = "friendly-alias"
	message := responseStateMessage()
	require.Len(t, client.replayResponse(message), 5, "display aliases do not scope opaque state")
	client.ModelConfig.Model = "gpt-6-astra"
	assert.Nil(t, client.replayResponse(message), "re-pinning an alias must not reuse another model's state")
}

func TestResponseStatePreservesMultipleAssistantPhases(t *testing.T) {
	t.Parallel()
	message := responseStateMessage()
	message.ToolCalls = nil
	message.Content = "Checking.Done."
	message.OpenAIResponse.Output = append(message.OpenAIResponse.Output[:2], json.RawMessage(`{"type":"message","id":"msg_2","role":"assistant","status":"completed","phase":"final_answer","content":[{"type":"output_text","text":"Done.","annotations":[]}]}`))
	items := responseStateClient().replayResponse(message)
	require.Len(t, items, 3)
	assert.Equal(t, responses.ResponseOutputMessagePhaseCommentary, items[1].OfOutputMessage.Phase)
	assert.Equal(t, responses.ResponseOutputMessagePhaseFinalAnswer, items[2].OfOutputMessage.Phase)
}

func TestResponseStateOrphanFunctionCall(t *testing.T) {
	t.Parallel()
	items := responseStateClient().convertMessagesToResponseInput(t.Context(), []chat.Message{responseStateMessage()})
	require.Len(t, items, 6)
	assert.Equal(t, "call_1", items[5].OfFunctionCallOutput.CallID.Value)
	assert.Contains(t, items[5].OfFunctionCallOutput.Output.OfString.Value, "not executed")
}

func TestResponseStateKeepsCompletedReasoningItem(t *testing.T) {
	t.Parallel()
	events := decodeEvents(t, []map[string]any{
		{"type": "response.output_item.done", "output_index": 0, "item": responseStateOutput()[0]},
		{"type": "response.completed", "response": map[string]any{"id": "resp_1", "output": []any{map[string]any{"type": "reasoning", "id": "rs_1", "summary": []any{}}}}},
	})
	adapter := responseStateClient().responseAdapter(t.Context(), &fakeEventStream{events: events})
	_, err := adapter.Recv()
	require.NoError(t, err)
	result, err := adapter.Recv()
	require.NoError(t, err)
	state := result.Choices[0].Delta.OpenAIResponse
	require.NotNil(t, state)
	require.Len(t, state.Output, 1)
	assert.JSONEq(t, string(responseStateOutput()[0]), string(state.Output[0]))
}

func TestResponseStateChatGPTDefaultEndpoint(t *testing.T) {
	t.Parallel()
	client := responseStateClient()
	client.ModelConfig.Provider = chatgpt.ProviderName
	client.ModelConfig.BaseURL = chatgpt.BaseURL
	message := responseStateMessage()
	message.OpenAIResponse.Source = "chatgpt/gpt-5.6"
	require.Len(t, client.replayResponse(message), 5)
	client.ModelConfig.BaseURL = "https://other.example/codex"
	assert.Nil(t, client.replayResponse(message))
}
