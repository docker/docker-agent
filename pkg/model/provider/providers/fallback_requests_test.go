package providers_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/config/latest"
)

// Server-side fallback is a beta of the Messages API: the request goes
// through the beta client with the July header, which accepts both forms.
func TestServerSideFallbackRequests(t *testing.T) {
	t.Parallel()

	env := map[string]string{"ANTHROPIC_API_KEY": "test-key"}
	for name, tc := range map[string]struct {
		fallbacks any
		want      any
	}{
		"default": {fallbacks: "default", want: "default"},
		"list":    {fallbacks: []any{"claude-sonnet-4-6"}, want: []any{map[string]any{"model": "claude-sonnet-4-6"}}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			req := captureFinalRequest(t, latest.ModelConfig{
				Provider: "anthropic", Model: "claude-opus-4-8",
				ProviderOpts: map[string]any{"fallbacks": tc.fallbacks},
			}, env)
			assert.Equal(t, "beta=true", req.query, "beta Messages API")
			assert.Contains(t, strings.Join(req.header.Values("anthropic-beta"), ","), "server-side-fallback-2026-07-01")
			assert.Equal(t, tc.want, req.body["fallbacks"])
		})
	}

	t.Run("none", func(t *testing.T) {
		t.Parallel()
		req := captureFinalRequest(t, latest.ModelConfig{Provider: "anthropic", Model: "claude-opus-4-8"}, env)
		assert.NotContains(t, req.body, "fallbacks")
		for _, beta := range req.header.Values("anthropic-beta") {
			assert.NotContains(t, beta, "server-side-fallback")
		}
	})
}

// Fallbacks route legacy configurations through the beta Messages API, which
// must send the same request as the standard path did: sampling settings when
// thinking is off, and no interleaved thinking when it was opted out.
func TestServerFallbackKeepsLegacyRequestShape(t *testing.T) {
	t.Parallel()

	env := map[string]string{"ANTHROPIC_API_KEY": "test-key"}
	t.Run("sampling without thinking", func(t *testing.T) {
		t.Parallel()
		req := captureFinalRequest(t, latest.ModelConfig{
			Provider: "anthropic", Model: "claude-sonnet-4-5", Temperature: new(0.3), TopP: new(0.9),
			ProviderOpts: map[string]any{"fallbacks": []any{"claude-haiku-4-5"}},
		}, env)
		assert.Equal(t, "beta=true", req.query)
		assert.Nil(t, req.body["thinking"])
		assert.InDelta(t, 0.3, req.body["temperature"], 1e-9)
		assert.InDelta(t, 0.9, req.body["top_p"], 1e-9)
	})

	t.Run("interleaved thinking opt-out", func(t *testing.T) {
		t.Parallel()
		req := captureFinalRequest(t, latest.ModelConfig{
			Provider: "anthropic", Model: "claude-sonnet-4-5", ThinkingBudget: &latest.ThinkingBudget{Tokens: 2048},
			ProviderOpts: map[string]any{"fallbacks": []any{"claude-haiku-4-5"}, "interleaved_thinking": false},
		}, env)
		assert.Equal(t, "beta=true", req.query)
		assert.NotContains(t, strings.Join(req.header.Values("anthropic-beta"), ","), "interleaved-thinking")
		assert.Nil(t, req.body["temperature"], "no sampling with thinking on")
	})
}

// A Claude 5.x chain goes out as configured: the server, not the client,
// adapts the request for each fallback attempt.
func TestServerFallbackClaude5Request(t *testing.T) {
	t.Parallel()

	req := captureFinalRequest(t, latest.ModelConfig{
		Provider: "anthropic", Model: "claude-sonnet-5-5",
		ProviderOpts: map[string]any{"fallbacks": []any{"claude-sonnet-5"}},
	}, map[string]string{"ANTHROPIC_API_KEY": "test-key"})

	assert.Equal(t, "beta=true", req.query)
	assert.Contains(t, strings.Join(req.header.Values("anthropic-beta"), ","), "server-side-fallback-2026-07-01")
	assert.Equal(t, []any{map[string]any{"model": "claude-sonnet-5"}}, req.body["fallbacks"])
	assert.Equal(t, "drop_block", field(req.body, "thinking", "block_binding", "prefix_mismatch_behavior"))
	assert.Equal(t, "adaptive", field(req.body, "thinking", "type"), "no client-side rewrite for the fallback")
}
