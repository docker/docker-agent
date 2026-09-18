package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/httpclient"
)

func TestResponseFeaturesAreOptIn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name             string
		opts             map[string]any
		metadata, output bool
	}{
		{name: "defaults"},
		{name: "explicitly disabled", opts: map[string]any{"preserve_reasoning": false, "native_tool_search": false, "cache_diagnostics": false}},
		{name: "invalid values", opts: map[string]any{"preserve_reasoning": "true", "native_tool_search": "true", "cache_diagnostics": "true"}},
		{name: "cache diagnostics only", opts: map[string]any{"cache_diagnostics": true}, metadata: true},
		{name: "preserve reasoning", opts: map[string]any{"preserve_reasoning": true}, metadata: true, output: true},
		{name: "native search", opts: map[string]any{"native_tool_search": true}, metadata: true, output: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := responseStateClient()
			client.ModelConfig.ProviderOpts = tc.opts
			var params responses.ResponseNewParams
			ctx := httpclient.ContextWithSessionID(t.Context(), "session_1")
			client.configureResponseState(ctx, &params, []chat.Message{responseStateMessage()})
			assert.Equal(t, tc.output, len(params.Include) > 0)
			assert.True(t, param.IsOmitted(params.PreviousResponseID), "SSE parameters must not acquire state chaining")
			assert.Equal(t, tc.output, len(client.replayResponse(responseStateMessage())) > 0)
			events := decodeEvents(t, []map[string]any{{"type": "response.completed", "response": map[string]any{"id": "resp_2", "output": responseStateOutput()}}})
			adapter := client.responseAdapter(ctx, &fakeEventStream{events: events})
			result, err := adapter.Recv()
			require.NoError(t, err)
			state := result.Choices[0].Delta.OpenAIResponse
			if !tc.metadata {
				assert.Nil(t, state)
				return
			}
			require.NotNil(t, state)
			assert.Equal(t, "resp_2", state.ID)
			assert.Equal(t, tc.output, len(state.Output) > 0, "diagnostics alone must not store output or change token estimates")
		})
	}
}

func TestDefaultResponsesWireIgnoresStoredReplayMetadata(t *testing.T) {
	t.Parallel()
	requests := make(chan map[string]json.RawMessage, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		requests <- body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_new\",\"output\":[]}}\n\n")
	}))
	t.Cleanup(server.Close)
	sdk := openai.NewClient(option.WithBaseURL(server.URL), option.WithAPIKey("test"))
	client := responseStateClient()
	client.ModelConfig.ProviderOpts = nil
	client.clientFn = func(context.Context) (*openai.Client, error) { return &sdk, nil }
	stream, err := client.CreateResponseStream(t.Context(), []chat.Message{responseStateMessage(), {Role: chat.MessageRoleTool, ToolCallID: "call_1", Content: "ok"}}, nil)
	require.NoError(t, err)
	defer stream.Close()
	drainReasoningTestStream(t, stream)
	body := <-requests
	assert.NotContains(t, body, "include")
	assert.NotContains(t, body, "prompt_cache_options")
	assert.NotContains(t, body, "previous_response_id")
	assert.JSONEq(t, `[{"role":"assistant","content":"Checking."},{"type":"function_call","call_id":"call_1","name":"read","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":"ok"}]`, string(body["input"]))
}

func TestRedactedResponseLoggingPreservesDiagnostics(t *testing.T) {
	t.Parallel()
	original := []byte(`{"model":"gpt-5.6","max_output_tokens":9007199254740993,"input":[{"type":"reasoning","encrypted_content":"opaque-secret","summary":[]},{"role":"user","content":"debug this"}],"tools":[{"name":"read","parameters":{"type":"object"}}],"nested":{"encrypted_content":"another-secret"}}`)
	got := redactEncryptedContent(original)
	assert.NotContains(t, got, "opaque-secret")
	assert.NotContains(t, got, "another-secret")
	assert.Contains(t, got, `"max_output_tokens":9007199254740993`)
	assert.Contains(t, got, `"parameters":{"type":"object"}`)
	assert.Contains(t, got, "debug this")
	assert.Contains(t, string(original), "opaque-secret", "logging must not mutate the request")
	const plain = `{"input":"hello","model":"gpt-5"}`
	assert.JSONEq(t, plain, redactEncryptedContent([]byte(plain)), "unchanged logging for legacy requests")
	assert.Equal(t, "<invalid JSON>", redactEncryptedContent([]byte(`{"encrypted_content":"do not leak"`)))
}

func TestIncompleteResponseRetainsLegacyFinishReason(t *testing.T) {
	t.Parallel()
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("preserve=%t", enabled), func(t *testing.T) {
			t.Parallel()
			client := responseStateClient()
			client.ModelConfig.ProviderOpts = map[string]any{"preserve_reasoning": enabled}
			events := decodeEvents(t, []map[string]any{{"type": "response.incomplete", "response": map[string]any{"id": "partial", "incomplete_details": map[string]any{"reason": "max_output_tokens"}, "output": []any{map[string]any{"type": "message", "id": "msg_1", "role": "assistant", "status": "in_progress", "phase": "final_answer", "content": []any{map[string]any{"type": "output_text", "text": "Partial", "annotations": []any{}}}}}}}})
			result, err := client.responseAdapter(t.Context(), &fakeEventStream{events: events}).Recv()
			require.NoError(t, err)
			assert.Equal(t, chat.FinishReasonLength, result.Choices[0].FinishReason)
			if !enabled {
				assert.Nil(t, result.Choices[0].Delta.OpenAIResponse)
				return
			}
			state := result.Choices[0].Delta.OpenAIResponse
			require.NotNil(t, state)
			message := chat.Message{Role: chat.MessageRoleAssistant, Content: "Partial", OpenAIResponse: state}
			replay := client.replayResponse(message)
			require.Len(t, replay, 1)
			assert.Equal(t, responses.ResponseOutputMessageStatusInProgress, replay[0].OfOutputMessage.Status)
		})
	}
}

func TestDefaultWebSocketContinuationIsUnchanged(t *testing.T) {
	t.Parallel()
	server, captured := testWSServerCapture(t, []map[string]any{completedEvent("resp_previous")})
	defer server.Close()
	client := responseStateClient()
	client.ModelConfig.ProviderOpts = nil
	client.wsPool = newWSPool(httpToWSURL(server.URL), func(context.Context) (http.Header, error) { return http.Header{}, nil })
	defer client.Close()
	for range 2 {
		stream, err := client.createWebSocketStream(t.Context(), defaultTestParams())
		require.NoError(t, err)
		drainStream(t, stream)
	}
	require.Len(t, *captured, 2)
	assertPreviousResponseID(t, (*captured)[0], "")
	assertPreviousResponseID(t, (*captured)[1], "resp_previous")

	client.ModelConfig.ProviderOpts = map[string]any{"preserve_reasoning": true}
	stream, err := client.createWebSocketStream(t.Context(), defaultTestParams())
	require.NoError(t, err)
	drainStream(t, stream)
	require.Len(t, *captured, 3)
	assert.JSONEq(t, `null`, string((*captured)[2]["previous_response_id"]))
}
