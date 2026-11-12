package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestRecoveredContentPreservesTextBoundaries(t *testing.T) {
	t.Parallel()

	for name, cfg := range map[string]latest.ModelConfig{
		"standard": {Provider: "anthropic", Model: "claude-sonnet-4-5", ThinkingBudget: &latest.ThinkingBudget{Tokens: 2048}, ProviderOpts: map[string]any{"interleaved_thinking": false}},
		"beta":     {Provider: "anthropic", Model: "claude-opus-4-8", ProviderOpts: map[string]any{"fallbacks": []any{"claude-sonnet-4-6"}}},
	} {
		for _, fallback := range []bool{false, true} {
			label := "interleaved"
			if fallback {
				label = "fallback"
			}
			t.Run(name+"/"+label, func(t *testing.T) {
				t.Parallel()
				prefix := `{"type":"text","text":"before "},`
				if fallback {
					prefix = `{"type":"thinking","thinking":"declined","signature":"old"},` + prefix +
						`{"type":"tool_use","id":"declined","name":"read","input":{}},` +
						`{"type":"fallback","from":{"model":"claude-opus-4-8"},"to":{"model":"claude-sonnet-4-6"},"trigger":{"type":"refusal","category":"cyber"}},`
				}
				raw := `[` + prefix + `{"type":"thinking","thinking":"plan","signature":"sig-intact"},` +
					`{"type":"redacted_thinking","data":"opaque"},{"type":"text","text":"middle","citations":[]},` +
					`{"type":"tool_use","id":"t1","name":"read\" path=\"x","input":{"token":"secret"}},` +
					`{"type":"text","text":" after"}]`
				turn := recordedTurn(raw, chat.ProviderStateVersion, chat.Message{
					Content: "before middle after", ReasoningContent: "plan", ThinkingSignature: "sig-intact",
					ToolCalls: []tools.ToolCall{{ID: "t1", Type: "function", Function: tools.FunctionCall{Name: "read", Arguments: `{"token":"[REDACTED]"}`}}},
				}, nil)
				before, err := json.Marshal(turn)
				require.NoError(t, err)
				srv := newScriptedServer(t, textReply("msg_next", "ok"))
				chatTurn(t, newScriptedClient(srv, cfg), []chat.Message{user("q"), turn, toolResult("t1")}, nil)

				want := string(projectServerFallback(json.RawMessage(raw), name == "beta"))
				want = strings.ReplaceAll(want, `read\" path=\"x`, "read")
				want = strings.ReplaceAll(want, `"secret"`, `"[REDACTED]"`)
				assert.JSONEq(t, want, rawJSON(t, withoutCacheControl(srv.request(t, 0).messages()[1]["content"])))
				after, err := json.Marshal(turn)
				require.NoError(t, err)
				assert.Equal(t, string(before), string(after), "conversion must not mutate recorded history")
			})
		}
	}
}

func assertRecoveredParams(t *testing.T, turn *chat.Message, want string, wantOK bool) {
	t.Helper()
	standard, ok := recoveredParams(turn)
	require.Equal(t, wantOK, ok)
	beta, ok := betaRecoveredParams(turn)
	require.Equal(t, wantOK, ok)
	if wantOK {
		assert.JSONEq(t, want, rawJSON(t, standard))
		assert.JSONEq(t, want, rawJSON(t, beta))
	}
}

func TestRecoveredContentTextEdits(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, text, want string
	}{
		{"single block", `{"type":"text","text":"old"},`, `[{"type":"thinking","thinking":"plan","signature":"sig"},{"type":"text","text":"edited"},{"type":"tool_use","id":"t1","name":"read","input":{}}]`},
		{"no block", "", `[{"type":"thinking","thinking":"plan","signature":"sig"},{"type":"text","text":"edited"},{"type":"tool_use","id":"t1","name":"read","input":{}}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			turn := recordedTurn(`[{"type":"thinking","thinking":"plan","signature":"sig"},`+tc.text+`{"type":"tool_use","id":"t1","name":"read","input":{}}]`, 1,
				chat.Message{Content: "edited", ReasoningContent: "plan", ToolCalls: []tools.ToolCall{readCall("t1")}}, nil)
			assertRecoveredParams(t, &turn, tc.want, true)
		})
	}

	t.Run("no text or tools", func(t *testing.T) {
		t.Parallel()
		turn := recordedTurn(`[{"type":"thinking","thinking":"plan","signature":"sig"}]`, 1,
			chat.Message{Content: "edited", ReasoningContent: "plan"}, nil)
		assertRecoveredParams(t, &turn, `[{"type":"thinking","thinking":"plan","signature":"sig"},{"type":"text","text":"edited"}]`, true)
	})

	for _, tc := range []struct{ name, raw string }{
		{"text before thinking", `[{"type":"text","text":"old"},{"type":"thinking","thinking":"plan","signature":"sig"},{"type":"tool_use","id":"t1","name":"read","input":{}}]`},
		{"text before redacted thinking", `[{"type":"text","text":"old"},{"type":"redacted_thinking","data":"opaque"},{"type":"thinking","thinking":"plan","signature":"sig"},{"type":"tool_use","id":"t1","name":"read","input":{}}]`},
		{"insertion before thinking", `[{"type":"tool_use","id":"t1","name":"read","input":{}},{"type":"thinking","thinking":"plan","signature":"sig"}]`},
		{"insertion before redacted thinking", `[{"type":"thinking","thinking":"plan","signature":"sig"},{"type":"tool_use","id":"t1","name":"read","input":{}},{"type":"redacted_thinking","data":"opaque"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			turn := recordedTurn(tc.raw, 1, chat.Message{Content: "edited", ReasoningContent: "plan", ToolCalls: []tools.ToolCall{readCall("t1")}}, nil)
			assertRecoveredParams(t, &turn, "", false)
		})
	}

	for name, cfg := range map[string]latest.ModelConfig{
		"standard": {Provider: "anthropic", Model: "claude-sonnet-4-5", ThinkingBudget: &latest.ThinkingBudget{Tokens: 2048}, ProviderOpts: map[string]any{"interleaved_thinking": false}},
		"beta":     {Provider: "anthropic", Model: "claude-fable-5-1"},
	} {
		t.Run(name+"/ambiguous text edit", func(t *testing.T) {
			t.Parallel()
			turn := recordedTurn(`[{"type":"text","text":"before"},{"type":"thinking","thinking":"plan","signature":"sig"},{"type":"text","text":"after"},{"type":"tool_use","id":"t1","name":"read","input":{}}]`, 1,
				chat.Message{Content: "edited", ReasoningContent: "plan", ToolCalls: []tools.ToolCall{readCall("t1")}}, nil)
			assertRecoveredParams(t, &turn, "", false)
			srv := newScriptedServer(t, textReply("msg_next", "ok"))
			client := newScriptedClient(srv, cfg)
			_, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{user("q"), turn, toolResult("t1")}, nil)
			require.ErrorContains(t, err, "cannot be replayed")
			assert.Zero(t, srv.count())
			chatTurn(t, client, []chat.Message{user("q"), turn, toolResult("t1"), user("next")}, nil)
			assert.JSONEq(t, `[{"type":"text","text":"edited"},{"type":"tool_use","id":"t1","name":"read","input":{}}]`,
				rawJSON(t, withoutCacheControl(srv.request(t, 0).messages()[1]["content"])))
		})
	}
}
