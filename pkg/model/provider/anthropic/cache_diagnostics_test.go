package anthropic

import (
	"encoding/json"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
)

func diagnosedReply(id, text string, reason map[string]any) string {
	return sseBody(
		messageStart(id, map[string]any{"diagnostics": map[string]any{"cache_miss_reason": reason}}),
		blockStart(0, map[string]any{"type": "text", "text": ""}),
		blockDelta(0, map[string]any{"type": "text_delta", "text": text}),
		blockStop(0),
		messageDelta("end_turn", nil),
		messageStop,
	)
}

func diagnosticsClient(srv *scriptedServer) *Client {
	return newScriptedClient(srv, latest.ModelConfig{
		Provider: "anthropic", Model: "claude-opus-4-7",
		ProviderOpts: map[string]any{"cache_diagnostics": true},
	})
}

// previousMessageID returns the wire value of diagnostics.previous_message_id
// and whether the key was sent at all: the first turn must send an explicit null.
func previousMessageID(t *testing.T, req capturedRequest) (any, bool) {
	t.Helper()
	diagnostics, ok := req.body["diagnostics"].(map[string]any)
	if !ok {
		return nil, false
	}
	id, present := diagnostics["previous_message_id"]
	return id, present
}

func TestCacheDiagnostics_FirstTurnNullThenPreviousMessageID(t *testing.T) {
	t.Parallel()
	srv := newScriptedServer(t,
		diagnosedReply("msg_1", "a1", nil),
		diagnosedReply("msg_2", "a2", map[string]any{"type": "system_changed", "cache_missed_input_tokens": 1200}),
	)
	client := diagnosticsClient(srv)

	a1 := chatTurn(t, client, []chat.Message{user("u1")}, nil)
	first := srv.request(t, 0)
	assert.Contains(t, first.betas, anthropic.AnthropicBetaCacheDiagnosis2026_04_07)
	id, present := previousMessageID(t, first)
	assert.True(t, present, "opt in with an explicit null: %s", first.raw)
	assert.Nil(t, id)
	require.NotNil(t, a1.ProviderState)
	assert.Equal(t, "msg_1", a1.ProviderState.MessageID)
	assert.Nil(t, a1.ProviderState.CacheDiagnostics, "a pending (null) diagnosis is not a miss")

	a2 := chatTurn(t, client, []chat.Message{user("u1"), a1, user("u2")}, nil)
	id, _ = previousMessageID(t, srv.request(t, 1))
	assert.Equal(t, "msg_1", id)
	require.NotNil(t, a2.ProviderState)
	assert.Equal(t, "msg_2", a2.ProviderState.MessageID)
	assert.Equal(t, &chat.CacheDiagnostics{Reason: "system_changed", MissedInputTokens: 1200}, a2.ProviderState.CacheDiagnostics)

	// The diagnosis survives the session round trip and the next request
	// chains from the latest reply.
	data, err := json.Marshal(a2)
	require.NoError(t, err)
	var restored chat.Message
	require.NoError(t, json.Unmarshal(data, &restored))
	assert.Equal(t, a2.ProviderState.CacheDiagnostics, restored.ProviderState.CacheDiagnostics)
	chatTurn(t, client, []chat.Message{user("u1"), a1, user("u2"), restored, user("u3")}, nil)
	id, _ = previousMessageID(t, srv.request(t, 2))
	assert.Equal(t, "msg_2", id)
}

func TestCacheDiagnostics_PreviousIDComesFromHistoryNotClient(t *testing.T) {
	t.Parallel()
	srv := newScriptedServer(t, diagnosedReply("msg_a", "a", nil), diagnosedReply("msg_b", "b", nil))
	client := diagnosticsClient(srv)

	ra := chatTurn(t, client, []chat.Message{user("conversation a")}, nil)
	rb := chatTurn(t, client, []chat.Message{user("conversation b")}, nil)
	assert.Equal(t, "msg_a", ra.ProviderState.MessageID)
	assert.Equal(t, "msg_b", rb.ProviderState.MessageID)
	for i := range 2 {
		id, present := previousMessageID(t, srv.request(t, i))
		assert.True(t, present)
		assert.Nil(t, id, "fresh histories never inherit an id from the shared client")
	}

	chatTurn(t, client, []chat.Message{user("conversation a"), ra, user("more a")}, nil)
	id, _ := previousMessageID(t, srv.request(t, 2))
	assert.Equal(t, "msg_a", id)
	chatTurn(t, client, []chat.Message{user("conversation b"), rb, user("more b")}, nil)
	id, _ = previousMessageID(t, srv.request(t, 3))
	assert.Equal(t, "msg_b", id)

	// An edited reply keeps its id: the server compares prompts, not text.
	edited := ra
	edited.Content = "edited by the user"
	chatTurn(t, client, []chat.Message{user("conversation a"), edited, user("again")}, nil)
	id, _ = previousMessageID(t, srv.request(t, 4))
	assert.Equal(t, "msg_a", id)

	// Foreign or id-less state falls back to the latest usable reply.
	foreign := chat.Message{Role: chat.MessageRoleAssistant, Content: "x"}
	foreign.AttachProviderState(&chat.ProviderState{Provider: "openai", MessageID: "resp_1", Content: json.RawMessage("[]")})
	chatTurn(t, client, []chat.Message{user("a"), ra, user("b"), foreign, user("c")}, nil)
	id, _ = previousMessageID(t, srv.request(t, 5))
	assert.Equal(t, "msg_a", id)
	chatTurn(t, client, []chat.Message{user("a"), foreign, user("c")}, nil)
	id, present := previousMessageID(t, srv.request(t, 6))
	assert.True(t, present)
	assert.Nil(t, id)
}

func TestCacheDiagnostics_OffByDefault(t *testing.T) {
	t.Parallel()
	srv := newScriptedServer(t, diagnosedReply("msg_1", "a1", map[string]any{"type": "tools_changed", "cache_missed_input_tokens": 9}))
	client := newScriptedClient(srv, latest.ModelConfig{
		Provider: "anthropic", Model: "claude-opus-4-7", ProviderOpts: map[string]any{"interleaved_thinking": true},
	})
	prior := chat.Message{Role: chat.MessageRoleAssistant, Content: "x"}
	prior.AttachProviderState(&chat.ProviderState{Provider: providerStateName, MessageID: "msg_0", Content: json.RawMessage(`[{"type":"text","text":"x"}]`)})

	a1 := chatTurn(t, client, []chat.Message{user("u0"), prior, user("u1")}, nil)

	req := srv.request(t, 0)
	assert.NotContains(t, req.betas, anthropic.AnthropicBetaCacheDiagnosis2026_04_07)
	assert.NotContains(t, req.body, "diagnostics")
	require.NotNil(t, a1.ProviderState)
	assert.Equal(t, &chat.CacheDiagnostics{Reason: "tools_changed", MissedInputTokens: 9}, a1.ProviderState.CacheDiagnostics,
		"a diagnosis the server volunteers is still surfaced")
}

func TestCacheDiagnostics_MissWithoutTokensKeepsReason(t *testing.T) {
	t.Parallel()
	srv := newScriptedServer(t, diagnosedReply("msg_1", "a1", map[string]any{"type": "previous_message_not_found"}))
	a1 := chatTurn(t, diagnosticsClient(srv), []chat.Message{user("u1")}, nil)
	require.NotNil(t, a1.ProviderState)
	assert.Equal(t, &chat.CacheDiagnostics{Reason: "previous_message_not_found"}, a1.ProviderState.CacheDiagnostics)
}

// Empty responses can still explain a cache miss.
func TestCacheDiagnostics_EmptyResponse(t *testing.T) {
	t.Parallel()
	srv := newScriptedServer(t, sseBody(
		messageStart("msg_1", map[string]any{"diagnostics": map[string]any{"cache_miss_reason": map[string]any{"type": "messages_changed", "cache_missed_input_tokens": 50}}}),
		messageDelta("end_turn", nil),
		messageStop,
	))
	a1 := chatTurn(t, diagnosticsClient(srv), []chat.Message{user("u1")}, nil)
	require.NotNil(t, a1.ProviderState)
	assert.Equal(t, "messages_changed", a1.ProviderState.CacheDiagnostics.Reason)
}
