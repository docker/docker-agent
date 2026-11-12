package anthropic

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/anthropic/anthropictest"
	"github.com/docker/docker-agent/pkg/model/provider/contracts"
)

func TestStickyServingModel(t *testing.T) {
	t.Parallel()

	const primary, target, later = "claude-opus-4-8", "claude-sonnet-4-6", "claude-haiku-4-5"
	for _, tc := range []struct {
		name, model, reply, early, final string
		opts                             map[string]any
		history                          string
	}{
		{name: "truncated explicit", model: primary, opts: map[string]any{"fallbacks": []string{target}}, reply: anthropictest.StickyTruncatedReply("m", target, "partial"), early: target, final: target},
		{name: "truncated history Claude 5", model: "claude-sonnet-5-5", history: target, reply: anthropictest.StickyTruncatedReply("m", target, "partial"), early: target, final: target},
		{name: "message-only usage", model: primary, opts: map[string]any{"fallbacks": []string{target}}, reply: anthropictest.StickyMessageUsageReply("m", target, "answer"), early: target},
		{name: "history alias collision", model: "claude-sonnet-5-5", history: "claude-sonnet-5-5-20261001", reply: anthropictest.StickyMessageUsageReply("m", "claude-sonnet-5-5-20261001", "answer"), early: "claude-sonnet-5-5-20261001"},
		{name: "ordinary Claude5 alias", model: "claude-sonnet-5-5", reply: anthropictest.StickyMessageUsageReply("m", "claude-sonnet-5-5-20261001", "answer")},
		{name: "ordinary Opus5 alias", model: "claude-opus-5", opts: map[string]any{"interleaved_thinking": true}, reply: anthropictest.StickyMessageUsageReply("m", "claude-opus-5-20260724", "answer")},
		{name: "ordinary alias", model: "claude-sonnet-4-6", opts: map[string]any{"interleaved_thinking": true}, reply: anthropictest.StickyMessageUsageReply("m", "claude-sonnet-4-6-20260217", "answer")},
		{name: "empty start", model: "claude-sonnet-5-5", reply: anthropictest.StickyTruncatedReply("m", "", "partial")},
		{name: "same model", model: "claude-sonnet-5-5", reply: anthropictest.StickyTruncatedReply("m", "claude-sonnet-5-5", "partial")},
		{name: "default is not explicit evidence", model: primary, opts: map[string]any{"fallbacks": "default"}, reply: anthropictest.StickyTruncatedReply("m", target, "partial")},
		{name: "unlisted target", model: primary, opts: map[string]any{"fallbacks": []string{target}}, reply: anthropictest.StickyTruncatedReply("m", later, "partial")},
		{name: "usage overrides provisional", model: primary, opts: map[string]any{"fallbacks": []string{target}}, reply: strings.Replace(anthropictest.StickyReply("m", later, "answer"), `"model":"`+later+`"`, `"model":"`+target+`"`, 1), early: target, final: later},
		{name: "marker overrides provisional and usage", model: primary, opts: map[string]any{"fallbacks": []string{target}}, reply: strings.ReplaceAll(strings.Replace(anthropictest.FallbackBeforeOutputReply("m", target, later, "cyber", "answer"), `"model":"`+later+`"`, `"model":"`+target+`"`, 1), `"model":"`+later+`","output_tokens":7,"type":"fallback_message"`, `"model":"`+target+`","output_tokens":7,"type":"fallback_message"`), early: target, final: later},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := newScriptedClient(newScriptedServer(t, tc.reply), latest.ModelConfig{
				Provider: "anthropic", Model: tc.model, ProviderOpts: tc.opts,
				DisplayModel: "configured-alias", Cost: &latest.CostConfig{Input: 100, Output: 200},
			})
			messages := []chat.Message{user("q")}
			if tc.history != "" {
				history := fallbackTurn(t, fmt.Sprintf(`[{"type":"fallback","from":{"model":"claude-opus-4-8"},"to":{"model":%q},"trigger":{"type":"refusal","category":"cyber"}},{"type":"text","text":"prior"}]`, tc.history), "prior", "", "")
				messages = append(messages, history, user("next"))
			}
			stream, err := client.CreateChatCompletionStream(t.Context(), messages, nil)
			require.NoError(t, err)
			defer stream.Close()
			served := stream.(contracts.ServedStream)
			_, ok := served.ServedBy()
			require.False(t, ok, "attribution must not precede message_start")
			_, err = stream.Recv()
			require.NoError(t, err)
			model, ok := served.ServedBy()
			assert.Equal(t, tc.early != "", ok)
			assert.Equal(t, tc.early, model.ID.Model)
			if ok {
				assert.Empty(t, model.Config.ModelConfig.DisplayModel)
				assert.Nil(t, model.Config.ModelConfig.Cost)
			}
			drainMarkers(t, stream)
			model, ok = served.ServedBy()
			assert.Equal(t, tc.final != "", ok)
			assert.Equal(t, tc.final, model.ID.Model)
			assert.Equal(t, "configured-alias", client.ID().Model)
			assert.Equal(t, tc.model, client.ModelConfig.Model)
			assert.NotNil(t, client.ModelConfig.Cost)
		})
	}
}

func TestStickyServingModelStaysPerStream(t *testing.T) {
	t.Parallel()
	const primary, target = "claude-opus-4-8", "claude-sonnet-4-6"
	client := fallbackClient(newScriptedServer(t,
		anthropictest.StickyTruncatedReply("sticky", target, "partial"),
		anthropictest.StickyTruncatedReply("ordinary", primary, "ordinary")))
	streams := make([]chat.MessageStream, 2)
	for i := range streams {
		stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{user("q")}, nil)
		require.NoError(t, err)
		t.Cleanup(stream.Close)
		streams[i] = stream
	}
	var wg sync.WaitGroup
	for i, stream := range streams {
		wg.Go(func() {
			drainMarkers(t, stream)
			model, ok := stream.(contracts.ServedStream).ServedBy()
			assert.Equal(t, i == 0, ok)
			if i == 0 {
				assert.Equal(t, target, model.ID.Model)
			}
		})
	}
	wg.Wait()
	assert.Equal(t, primary, client.ID().Model)
}
