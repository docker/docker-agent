package anthropic

import (
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	ragtypes "github.com/docker/docker-agent/pkg/rag/types"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestHaiku55RequestPolicy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		budget *latest.ThinkingBudget
		opts   []options.Opt
		effort string
		off    bool
	}{
		{name: "default"},
		{name: "tokens", budget: &latest.ThinkingBudget{Tokens: 16000}, effort: "medium"},
		{name: "zero", budget: &latest.ThinkingBudget{}, off: true},
		{name: "none", budget: &latest.ThinkingBudget{Effort: "none"}, off: true},
		{name: "NoThinking overrides configured effort", budget: &latest.ThinkingBudget{Effort: "max"}, opts: []options.Opt{options.WithNoThinking(), options.WithMaxTokens(20)}, off: true},
		{name: "NoThinking default", opts: []options.Opt{options.WithNoThinking()}, off: true},
		{name: "low", budget: &latest.ThinkingBudget{Effort: "low"}, effort: "low"},
		{name: "medium", budget: &latest.ThinkingBudget{Effort: "medium"}, effort: "medium"},
		{name: "high", budget: &latest.ThinkingBudget{Effort: "high"}, effort: "high"},
		{name: "xhigh", budget: &latest.ThinkingBudget{Effort: "xhigh"}, effort: "xhigh"},
		{name: "max", budget: &latest.ThinkingBudget{Effort: "max"}, effort: "max"},
		{name: "adaptive", budget: &latest.ThinkingBudget{Effort: "adaptive"}, effort: "high"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := newScriptedServer(t, textReply("ok", "ok"))
			cfg := latest.ModelConfig{Provider: "anthropic", Model: "claude-haiku-5-5", ThinkingBudget: tc.budget, Temperature: new(0.3), TopP: new(0.8), ProviderOpts: samplingOpts(map[string]any{"thinking_display": "summarized"})}
			before := haikuJSON(t, cfg)
			client := newScriptedClient(srv, cfg, tc.opts...)
			chatTurn(t, client, []chat.Message{user("hello")}, nil)
			req := srv.request(t, 0)
			for _, key := range []string{"temperature", "top_p", "top_k"} {
				assert.NotContains(t, req.body, key)
			}
			assert.NotContains(t, req.thinking(), "budget_tokens")
			if tc.off {
				assert.Equal(t, map[string]any{"type": "disabled"}, req.thinking())
				assert.NotContains(t, req.betas, anthropic.AnthropicBetaThinkingBindingControls2026_08_01)
			} else {
				assert.Equal(t, "adaptive", req.thinking()["type"])
				assert.Equal(t, "summarized", req.thinking()["display"])
				assert.Equal(t, map[string]any{"prefix_mismatch_behavior": "drop_block"}, req.thinking()["block_binding"])
				assert.Contains(t, req.betas, anthropic.AnthropicBetaThinkingBindingControls2026_08_01)
			}
			if tc.effort == "" {
				assert.NotContains(t, req.body, "output_config")
			} else {
				assert.Equal(t, tc.effort, req.outputConfig()["effort"])
			}
			assert.Equal(t, before, haikuJSON(t, cfg))
		})
	}
}

func TestHaiku55HistoryTransitions(t *testing.T) {
	t.Parallel()
	msg := drainStandard(t, interleavedThinkingEvents(true))
	raw, err := json.Marshal(msg)
	require.NoError(t, err)
	var reloaded chat.Message
	require.NoError(t, json.Unmarshal(raw, &reloaded))
	messages := []chat.Message{
		system("edited system"), user("compacted earlier prefix"), reloaded,
		{Role: chat.MessageRoleTool, ToolCallID: "toolu_A", Content: "file"},
		{Role: chat.MessageRoleTool, ToolCallID: "toolu_B", Content: "dir"},
	}
	before := haikuJSON(t, messages)
	// Both converters must remove reasoning only on outgoing disabled turns.
	for _, beta := range []bool{false, true} {
		t.Run(map[bool]string{false: "standard", true: "beta"}[beta], func(t *testing.T) {
			client := clientWithModel("claude-haiku-5-5", &latest.ThinkingBudget{Effort: "none"}, nil)
			convert := func() []map[string]any {
				if beta {
					got, err := client.convertBetaMessages(t.Context(), messages)
					require.NoError(t, err)
					return wireMessages(haikuWire(t, map[string]any{"messages": got}))
				}
				got, err := client.convertMessages(t.Context(), messages)
				require.NoError(t, err)
				return wireMessages(haikuWire(t, map[string]any{"messages": got}))
			}
			off := convert()
			assert.NotContains(t, string(haikuJSON(t, off)), `"thinking"`)
			assert.NotContains(t, string(haikuJSON(t, off)), `"redacted_thinking"`)
			client.ModelConfig.ThinkingBudget = nil
			adaptive := convert()
			blocks := adaptive[1]["content"].([]any)
			var visible []any
			for _, b := range blocks {
				block := b.(map[string]any)
				if block["type"] != "thinking" && block["type"] != "redacted_thinking" {
					visible = append(visible, b)
				}
			}
			assert.Equal(t, visible, off[1]["content"])
			assert.Equal(t, adaptive[2], off[2], "tool result pairing is unchanged")
			assert.Equal(t, before, haikuJSON(t, messages))
		})
	}
	// Request-level validation leaves a valid tool-result ending intact, with
	// changed tools/system/history protected by drop_block, not local rewrites.
	srv := newScriptedServer(t, textReply("ok", "ok"))
	client := newScriptedClient(srv, latest.ModelConfig{Provider: "anthropic", Model: "claude-haiku-5-5"})
	chatTurn(t, client, messages, []tools.Tool{{Name: "read_file", Description: "edited tool", Parameters: map[string]any{"type": "object"}}})
	assert.Equal(t, "drop_block", srv.request(t, 0).thinking()["block_binding"].(map[string]any)["prefix_mismatch_behavior"])
}

func TestHaiku55PrefillAndFallbackValidation(t *testing.T) {
	t.Parallel()
	srv := newScriptedServer(t, textReply("ok", "ok"))
	for _, budget := range []*latest.ThinkingBudget{nil, {Effort: "none"}} {
		client := newScriptedClient(srv, latest.ModelConfig{Provider: "anthropic", Model: "claude-haiku-5-5", ThinkingBudget: budget})
		_, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{user("hello"), {Role: chat.MessageRoleAssistant, Content: "prefill"}}, nil)
		require.ErrorContains(t, err, "rejects assistant prefill")
	}
	assert.Zero(t, srv.count())
	for _, cfg := range []latest.ModelConfig{
		{Provider: "anthropic", Model: "claude-haiku-5-5", ProviderOpts: map[string]any{"fallbacks": []string{"claude-sonnet-5"}}},
		{Provider: "anthropic", Model: "claude-sonnet-5", ProviderOpts: map[string]any{"fallbacks": []string{"claude-haiku-5-5"}}},
	} {
		_, err := NewClient(t.Context(), &cfg, environment.NewMapEnvProvider(map[string]string{"ANTHROPIC_API_KEY": "test"}))
		require.ErrorContains(t, err, "server-side fallbacks")
		require.ErrorContains(t, err, "client-side routing")
	}
	require.NoError(t, validateThinkingOptions(&latest.ModelConfig{Model: "claude-haiku-5-5", ProviderOpts: map[string]any{"fallbacks": []string{}}}))
	require.Error(t, validateThinkingOptions(&latest.ModelConfig{Model: "claude-haiku-5-5", ProviderOpts: map[string]any{"thinking_display": "display"}}))
}

func TestHaiku55RerankOmitsSampling(t *testing.T) {
	t.Parallel()
	srv := newScriptedServer(t, textReply("ok", `{"scores":[0.7]}`))
	client := newScriptedClient(srv, latest.ModelConfig{Provider: "anthropic", Model: "claude-haiku-5-5", Temperature: new(0.2), TopP: new(0.9), ProviderOpts: samplingOpts(nil)})
	_, err := client.Rerank(t.Context(), "query", []ragtypes.Document{{Content: "doc"}}, "")
	require.NoError(t, err)
	for _, key := range []string{"temperature", "top_p", "top_k"} {
		assert.NotContains(t, srv.request(t, 0).body, key)
	}
}

func haikuJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)
	return data
}

func haikuWire(t *testing.T, value any) map[string]any {
	t.Helper()
	var result map[string]any
	require.NoError(t, json.Unmarshal(haikuJSON(t, value), &result))
	return result
}

func TestHaiku55StandardThinkingConfig(t *testing.T) {
	t.Parallel()
	for _, budget := range []*latest.ThinkingBudget{nil, {Tokens: 16000}, {Effort: "none"}} {
		c := clientWithModel("claude-haiku-5-5", budget, nil)
		params := anthropic.MessageNewParams{}
		enabled := c.applyThinkingConfig(&params, 8192)
		body := haikuWire(t, params)
		if budget != nil && budget.IsDisabled() {
			assert.False(t, enabled)
			assert.Equal(t, map[string]any{"type": "disabled"}, body["thinking"])
		} else {
			assert.True(t, enabled)
			assert.Equal(t, "adaptive", body["thinking"].(map[string]any)["type"])
		}
		if budget == nil {
			assert.NotContains(t, body, "output_config")
		}
	}
}

func TestHaiku55OmittedDisplayAndExplicitBinding(t *testing.T) {
	t.Parallel()
	srv := newScriptedServer(t, textReply("ok", "ok"))
	c := newScriptedClient(srv, latest.ModelConfig{Provider: "anthropic", Model: "claude-haiku-5-5", ProviderOpts: map[string]any{"thinking_display": "omitted", "thinking_prefix_mismatch": "error"}})
	chatTurn(t, c, []chat.Message{user("hello")}, nil)
	thinking := srv.request(t, 0).thinking()
	assert.Equal(t, "omitted", thinking["display"])
	assert.Equal(t, map[string]any{"prefix_mismatch_behavior": "error"}, thinking["block_binding"])
}

func TestHaiku55Refusal(t *testing.T) {
	t.Parallel()
	srv := newScriptedServer(t, sseBody(messageStart("refused", nil), messageDelta("refusal", nil), messageStop))
	c := newScriptedClient(srv, latest.ModelConfig{Provider: "anthropic", Model: "claude-haiku-5-5"})
	stream, err := c.CreateChatCompletionStream(t.Context(), []chat.Message{user("test")}, nil)
	require.NoError(t, err)
	defer stream.Close()
	var reason chat.FinishReason
	for {
		response, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		for _, choice := range response.Choices {
			if choice.FinishReason != "" {
				reason = choice.FinishReason
			}
		}
	}
	assert.Equal(t, chat.FinishReasonRefusal, reason)
}
