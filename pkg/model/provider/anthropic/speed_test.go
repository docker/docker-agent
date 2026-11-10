package anthropic

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/modelsdev/embedded"
	"github.com/docker/docker-agent/pkg/rag/types"
	"github.com/docker/docker-agent/pkg/runtime/pricing"
)

func TestValidateSpeed(t *testing.T) {
	t.Parallel()

	for name, opts := range map[string]map[string]any{
		"unset":    nil,
		"empty":    {"speed": ""},
		"fast":     {"speed": "fast"},
		"standard": {"speed": "standard"},
	} {
		require.NoError(t, validateSpeed(&latest.ModelConfig{ProviderOpts: opts}), name)
	}
	for name, value := range map[string]any{
		"unknown":        "turbo",
		"wrong case":     "Fast",
		"bool":           true,
		"number":         1,
		"nil":            nil,
		"padded":         " fast",
		"fast with typo": "fats",
	} {
		err := validateSpeed(&latest.ModelConfig{ProviderOpts: map[string]any{"speed": value}})
		require.EqualError(t, err, "anthropic: speed must be fast or standard", name)
	}
}

func TestNewClient_RejectsInvalidSpeed(t *testing.T) {
	t.Parallel()

	cfg := &latest.ModelConfig{Provider: "anthropic", Model: "claude-opus-5-5", ProviderOpts: map[string]any{"speed": "turbo"}}
	env := environment.NewMapEnvProvider(map[string]string{"ANTHROPIC_API_KEY": "test-key"})

	_, err := NewClient(t.Context(), cfg, env)
	require.ErrorContains(t, err, "speed must be fast or standard")

	factoryCalled := false
	_, err = NewClientFromFactoryWithOptions(t.Context(), cfg, env, func(context.Context, options.ModelOptions) (anthropic.Client, error) {
		factoryCalled = true
		return anthropic.Client{}, nil
	})
	require.ErrorContains(t, err, "speed must be fast or standard")
	assert.False(t, factoryCalled)
}

func TestApplySpeed(t *testing.T) {
	t.Parallel()

	t.Run("fast sets the parameter and appends the beta", func(t *testing.T) {
		t.Parallel()
		params := anthropic.BetaMessageNewParams{Betas: []anthropic.AnthropicBeta{"existing"}}
		applySpeed(&params, map[string]any{"speed": "fast"})

		assert.Equal(t, anthropic.BetaMessageNewParamsSpeedFast, params.Speed)
		assert.Equal(t, []anthropic.AnthropicBeta{"existing", anthropic.AnthropicBetaFastMode2026_02_01}, params.Betas)
	})

	for name, opts := range map[string]map[string]any{
		"standard": {"speed": "standard"},
		"empty":    {"speed": ""},
		"unset":    nil,
	} {
		t.Run(name+" leaves the request untouched", func(t *testing.T) {
			t.Parallel()
			params := anthropic.BetaMessageNewParams{Betas: []anthropic.AnthropicBeta{"existing"}}
			applySpeed(&params, opts)

			assert.Empty(t, params.Speed)
			assert.Equal(t, []anthropic.AnthropicBeta{"existing"}, params.Betas)
			raw, err := json.Marshal(params)
			require.NoError(t, err)
			assert.NotContains(t, string(raw), `"speed"`)
		})
	}
}

func TestServedTier(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "fast", servedTier(anthropic.BetaUsageSpeedFast))
	assert.Empty(t, servedTier(anthropic.BetaUsageSpeedStandard))
	assert.Empty(t, servedTier(""))
	assert.Equal(t, "fast", betaUsageFromMessage(anthropic.BetaUsage{Speed: anthropic.BetaUsageSpeedFast}).ServiceTier)
	assert.Empty(t, betaUsageFromMessage(anthropic.BetaUsage{Speed: anthropic.BetaUsageSpeedStandard}).ServiceTier)
}

func TestFastMode_Requests(t *testing.T) {
	t.Parallel()

	t.Run("fast routes through the beta API with speed and beta header", func(t *testing.T) {
		t.Parallel()
		srv := newScriptedServer(t, textReply("msg_1", "ok"))
		client := newScriptedClient(srv, latest.ModelConfig{
			Provider: "anthropic", Model: "claude-opus-4-8", ProviderOpts: map[string]any{"speed": "fast"},
		})
		chatTurn(t, client, []chat.Message{user("hello")}, nil)

		req := srv.request(t, 0)
		assert.Equal(t, "fast", req.body["speed"])
		assert.Equal(t, 1, countOf(req.betas, anthropic.AnthropicBetaFastMode2026_02_01))
	})

	t.Run("rerank carries speed and the beta once", func(t *testing.T) {
		t.Parallel()
		srv := newScriptedServer(t, textReply("msg_r", `{"scores":[0.5]}`))
		client := newScriptedClient(srv, latest.ModelConfig{
			Provider: "anthropic", Model: "claude-opus-4-8", ProviderOpts: map[string]any{"speed": "fast"},
		})

		scores, err := client.Rerank(t.Context(), "query", []types.Document{{Content: "doc"}}, "")
		require.NoError(t, err)
		assert.Equal(t, []float64{0.5}, scores)

		req := srv.request(t, 0)
		assert.Equal(t, "fast", req.body["speed"])
		assert.Equal(t, 1, countOf(req.betas, anthropic.AnthropicBetaFastMode2026_02_01))
		assert.Contains(t, req.betas, "structured-outputs-2025-11-13")
	})

	for name, opts := range map[string]map[string]any{
		"standard": {"speed": "standard"},
		"unset":    nil,
	} {
		t.Run(name+" keeps the non-beta request", func(t *testing.T) {
			t.Parallel()
			srv := newScriptedServer(t, textReply("msg_1", "ok"))
			client := newScriptedClient(srv, latest.ModelConfig{Provider: "anthropic", Model: "claude-opus-4-8", ProviderOpts: opts})
			chatTurn(t, client, []chat.Message{user("hello")}, nil)

			req := srv.request(t, 0)
			assert.NotContains(t, req.body, "speed")
			assert.Equal(t, []string{"fine-grained-tool-streaming-2025-05-14"}, req.betas)
		})
	}

	t.Run("compaction carries speed and bills the served tier", func(t *testing.T) {
		t.Parallel()
		reply := sseBody(
			messageStart("msg_c", map[string]any{"usage": map[string]any{"input_tokens": 0, "output_tokens": 0, "speed": "fast"}}),
			blockStart(0, map[string]any{"type": "compaction", "content": "S", "encrypted_content": "e", "signature": "s"}),
			blockStop(0),
			messageDelta("compaction", map[string]any{"output_tokens": 0, "iterations": []any{
				map[string]any{"type": "compaction", "input_tokens": 1000, "output_tokens": 200},
			}}),
			messageStop,
		)
		srv := newScriptedServer(t, reply)
		client := compactionClient(srv, map[string]any{"speed": "fast"})

		result, err := client.CompactConversation(t.Context(), []chat.Message{user("u")}, nil, "")
		require.NoError(t, err)
		require.NotNil(t, result)

		req := srv.request(t, 0)
		assert.Equal(t, "fast", req.body["speed"])
		assert.Contains(t, req.betas, anthropic.AnthropicBetaFastMode2026_02_01)
		require.Len(t, result.Iterations, 1)
		assert.Equal(t, "fast", result.Iterations[0].ServiceTier)
	})
}

func countOf(betas []string, beta anthropic.AnthropicBeta) int {
	count := 0
	for _, b := range betas {
		if b == beta {
			count++
		}
	}
	return count
}

func TestNewClient_RecordsEnvironmentEndpointForPricing(t *testing.T) {
	cfg := &latest.ModelConfig{Provider: "anthropic", Model: "claude-opus-5-5"}
	env := environment.NewMapEnvProvider(map[string]string{"ANTHROPIC_API_KEY": "test-key"})
	store := modelsdev.NewDatabaseStore(embedded.Snapshot())
	id := modelsdev.NewID("anthropic", "claude-opus-5-5")
	model, err := store.GetModel(t.Context(), id)
	require.NoError(t, err)
	usage := &chat.Usage{InputTokens: 1_000_000, ServiceTier: "fast"}
	inputPrice := func(client *Client) float64 {
		priced := pricing.ApplyModelCost(model, id, usage, client.Config)
		return *pricing.ComputeMessageCost(usage, priced)
	}

	official, err := NewClient(t.Context(), cfg, env)
	require.NoError(t, err)
	assert.InDelta(t, 2*model.Cost.Input, inputPrice(official), 1e-9)

	t.Setenv("ANTHROPIC_BASE_URL", "https://proxy.example.com")
	proxied, err := NewClient(t.Context(), cfg, env)
	require.NoError(t, err)
	assert.Equal(t, "https://proxy.example.com", proxied.Endpoint())
	assert.InDelta(t, model.Cost.Input, inputPrice(proxied), 1e-9)
}
