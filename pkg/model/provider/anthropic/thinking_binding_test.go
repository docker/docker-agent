package anthropic

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
)

func TestThinkingBindingBehavior(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		model string
		opts  map[string]any
		want  string
	}{
		"fable 5.1":             {model: "claude-fable-5-1", want: "drop_block"},
		"fable 5.1 dated":       {model: "claude-fable-5-1-20260901", want: "drop_block"},
		"fable 5.2":             {model: "claude-fable-5-2", want: "drop_block"},
		"mythos 5.1":            {model: "claude-mythos-5-1", want: "drop_block"},
		"gateway qualified":     {model: "anthropic/claude-fable-5-1", want: "drop_block"},
		"vertex":                {model: "claude-fable-5-1@20260901", want: "drop_block"},
		"fable 5":               {model: "claude-fable-5"},
		"mythos 5":              {model: "claude-mythos-5"},
		"sonnet 5":              {model: "claude-sonnet-5"},
		"opus 5":                {model: "claude-opus-5"},
		"fallback checks":       {model: "claude-opus-4-7", opts: map[string]any{"fallbacks": []any{"claude-sonnet-5", "claude-fable-5-1"}}, want: "drop_block"},
		"explicit error":        {model: "claude-fable-5-1", opts: map[string]any{"thinking_prefix_mismatch": "error"}, want: "error"},
		"explicit on old":       {model: "claude-sonnet-4-6", opts: map[string]any{"thinking_prefix_mismatch": "drop_block"}, want: "drop_block"},
		"non-string is ignored": {model: "claude-sonnet-4-6", opts: map[string]any{"thinking_prefix_mismatch": 1}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := &Client{}
			c.ModelConfig = latest.ModelConfig{Provider: "anthropic", Model: tc.model, ProviderOpts: tc.opts}
			assert.Equal(t, tc.want, c.thinkingBindingBehavior())
		})
	}
}

func TestApplyThinkingBinding(t *testing.T) {
	t.Parallel()
	drop := anthropic.BetaThinkingBlockBindingParam{PrefixMismatchBehavior: anthropic.BetaThinkingPrefixMismatchBehaviorDropBlock}

	t.Run("adaptive", func(t *testing.T) {
		t.Parallel()
		c := clientWithModel("claude-fable-5-1", &latest.ThinkingBudget{Effort: "high"}, nil)
		params := anthropic.BetaMessageNewParams{Betas: []anthropic.AnthropicBeta{"existing"}}
		c.applyBetaThinkingConfig(&params, 8192)
		c.applyThinkingBinding(&params)
		require.NotNil(t, params.Thinking.OfAdaptive)
		assert.Equal(t, drop, params.Thinking.OfAdaptive.BlockBinding)
		assert.Equal(t, []anthropic.AnthropicBeta{"existing", anthropic.AnthropicBetaThinkingBindingControls2026_08_01}, params.Betas)
	})

	t.Run("enabled", func(t *testing.T) {
		t.Parallel()
		c := clientWithModel("claude-sonnet-4-5", &latest.ThinkingBudget{Tokens: 2048}, map[string]any{"thinking_prefix_mismatch": "drop_block"})
		params := anthropic.BetaMessageNewParams{}
		c.applyBetaThinkingConfig(&params, 8192)
		c.applyThinkingBinding(&params)
		require.NotNil(t, params.Thinking.OfEnabled)
		assert.Equal(t, drop, params.Thinking.OfEnabled.BlockBinding)
		assert.Contains(t, params.Betas, anthropic.AnthropicBetaThinkingBindingControls2026_08_01)
	})

	t.Run("thinking omitted is not turned on", func(t *testing.T) {
		t.Parallel()
		c := clientWithModel("claude-sonnet-4-6", nil, map[string]any{"fallbacks": []any{"claude-fable-5-1"}})
		params := anthropic.BetaMessageNewParams{}
		c.applyBetaThinkingConfig(&params, 8192)
		c.applyThinkingBinding(&params)
		assert.Nil(t, params.Thinking.OfAdaptive)
		assert.Nil(t, params.Thinking.OfEnabled)
		assert.Contains(t, params.Betas, anthropic.AnthropicBetaThinkingBindingControls2026_08_01)
	})

	t.Run("thinking disabled", func(t *testing.T) {
		t.Parallel()
		c := clientWithModel("claude-sonnet-4-6", &latest.ThinkingBudget{Effort: "none"}, map[string]any{"thinking_prefix_mismatch": "error"})
		params := anthropic.BetaMessageNewParams{}
		c.applyBetaThinkingConfig(&params, 8192)
		c.applyThinkingBinding(&params)
		require.NotNil(t, params.Thinking.OfDisabled)
		body := wire(t, params)
		assert.Equal(t, map[string]any{"type": "disabled"}, body["thinking"])
	})

	t.Run("no behavior leaves params alone", func(t *testing.T) {
		t.Parallel()
		c := clientWithModel("claude-opus-4-7", &latest.ThinkingBudget{Effort: "high"}, nil)
		params := anthropic.BetaMessageNewParams{}
		c.applyBetaThinkingConfig(&params, 8192)
		before := wireBytes(t, params)
		c.applyThinkingBinding(&params)
		assert.Equal(t, before, wireBytes(t, params))
		assert.Empty(t, params.Betas)
	})
}

func TestThinkingTransformations(t *testing.T) {
	t.Parallel()
	var msg anthropic.BetaMessage
	require.NoError(t, msg.UnmarshalJSON([]byte(`{"id":"m","type":"message","role":"assistant","content":[],"input_transformations":[
		{"type":"thinking_dropped","path":"messages.1.content.0","reason":"prefix_mismatch"},
		{"type":"thinking_mismatch_allowed","path":"messages.3.content.0","reason":"prefix_mismatch"},
		{"type":"thinking_dropped","path":"messages.5.content.1","reason":"conversation_mismatch"}
	]}`)))
	assert.Equal(t, []string{
		"messages.1.content.0: prefix_mismatch",
		"messages.5.content.1: conversation_mismatch",
	}, thinkingTransformations(msg.InputTransformations))
	assert.Nil(t, thinkingTransformations(nil))
}

// Sequential on purpose: it swaps the default logger to observe the warning.
func TestBetaStream_WarnsOnDroppedThinking(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	srv := newScriptedServer(t, sseBody(
		messageStart("msg_1", map[string]any{"input_transformations": []any{
			map[string]any{"type": "thinking_dropped", "path": "messages.1.content.0", "reason": "prefix_mismatch"},
		}}),
		blockStart(0, map[string]any{"type": "text", "text": ""}),
		blockDelta(0, map[string]any{"type": "text_delta", "text": "still answering"}),
		blockStop(0),
		messageDelta("end_turn", nil),
		messageStop,
	))
	client := newScriptedClient(srv, latest.ModelConfig{Provider: "anthropic", Model: "claude-fable-5-1"})
	prior := chat.Message{Role: chat.MessageRoleAssistant, Content: "a1", ReasoningContent: "old plan"}
	prior.AttachProviderState(&chat.ProviderState{Provider: providerStateName, Content: []byte(`[{"type":"thinking","thinking":"old plan","signature":"stale"},{"type":"text","text":"a1"}]`)})

	reply := chatTurn(t, client, []chat.Message{user("u1"), prior, user("u2")}, nil)

	assert.Equal(t, "still answering", reply.Content, "the stream is not interrupted")
	require.NotNil(t, reply.ProviderState)
	assert.Contains(t, logs.String(), "Anthropic dropped invalidated thinking")
	assert.Contains(t, logs.String(), "messages.1.content.0: prefix_mismatch")
	req := srv.request(t, 0)
	assert.Equal(t, "drop_block", req.thinking()["block_binding"].(map[string]any)["prefix_mismatch_behavior"])
	assert.Contains(t, req.raw, `"signature":"stale"`, "the stale block is still sent; the server decides to drop it")
}

func TestHaiku55_WarnsOnDroppedThinkingAfterReenable(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	srv := newScriptedServer(t, sseBody(
		messageStart("msg_1", map[string]any{"input_transformations": []any{
			map[string]any{"type": "thinking_dropped", "path": "messages.1.content.0", "reason": "prefix_mismatch"},
		}}),
		blockStart(0, map[string]any{"type": "text", "text": ""}),
		blockDelta(0, map[string]any{"type": "text_delta", "text": "still answering"}),
		blockStop(0),
		messageDelta("end_turn", nil),
		messageStop,
	))
	client := newScriptedClient(srv, latest.ModelConfig{Provider: "anthropic", Model: "claude-haiku-5-5", ThinkingBudget: &latest.ThinkingBudget{Effort: "none"}})
	prior := chat.Message{Role: chat.MessageRoleAssistant, Content: "a1", ReasoningContent: "old plan"}
	prior.AttachProviderState(&chat.ProviderState{Provider: providerStateName, Content: []byte(`[{"type":"thinking","thinking":"old plan","signature":"stale"},{"type":"text","text":"a1"}]`)})

	chatTurn(t, client, []chat.Message{user("u1"), prior, user("u2")}, nil)
	assert.NotContains(t, srv.request(t, 0).raw, `"signature":"stale"`)
	client.ModelConfig.ThinkingBudget = nil
	reply := chatTurn(t, client, []chat.Message{user("u1"), prior, user("u2")}, nil)

	assert.Equal(t, "still answering", reply.Content, "the stream is not interrupted")
	require.NotNil(t, reply.ProviderState)
	assert.Contains(t, logs.String(), "Anthropic dropped invalidated thinking")
	assert.Contains(t, logs.String(), "messages.1.content.0: prefix_mismatch")
	req := srv.request(t, 1)
	assert.Equal(t, "drop_block", req.thinking()["block_binding"].(map[string]any)["prefix_mismatch_behavior"])
	assert.Contains(t, req.raw, `"signature":"stale"`, "the stale block is still sent; the server decides to drop it")
}
