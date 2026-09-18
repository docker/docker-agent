package openai

import (
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/concurrent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/tools"
)

// toolsForReasoningTest returns a minimal single-tool request, enough to
// exercise the tools-present branch of the Chat Completions reasoning-effort
// gate without pulling in schema-conversion edge cases.
func toolsForReasoningTest() []tools.Tool {
	return []tools.Tool{
		{
			Name:        "get_time",
			Description: "get the time",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	}
}

// chatCompletionsReasoningEffortField drives a Chat Completions request for
// model with tools against a mock server, forcing Chat Completions via
// api_type, and returns the raw request body plus whether it carried the
// reasoning_effort key at all (as opposed to the field being present but
// empty).
func chatCompletionsReasoningEffortField(t *testing.T, model string, toolList []tools.Tool) map[string]any {
	t.Helper()

	server, body := captureRequestBody(t)
	cfg := &latest.ModelConfig{
		Provider:       "openai",
		Model:          model,
		BaseURL:        server.URL,
		TokenKey:       "MY_TOKEN",
		ThinkingBudget: &latest.ThinkingBudget{Effort: "high"},
		// Force Chat Completions even for a model that would otherwise
		// auto-select the Responses API.
		ProviderOpts: map[string]any{"api_type": "openai_chatcompletions"},
	}
	env := environment.NewMapEnvProvider(map[string]string{"MY_TOKEN": "secret"})

	client, err := NewClient(t.Context(), cfg, env)
	require.NoError(t, err)

	stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hi"}}, toolList)
	require.NoError(t, err)
	defer stream.Close()
	drainReasoningTestStream(t, stream)

	var req map[string]any
	require.NoError(t, json.Unmarshal(body(), &req))
	return req
}

// chatCompletionsRequest drives a Chat Completions request for model with
// tools against a mock server, forcing Chat Completions via api_type, using
// the given request options (e.g. options.WithNoThinking()), with no
// thinking_budget configured. Returns the raw request body.
func chatCompletionsRequest(t *testing.T, model string, toolList []tools.Tool, opts ...options.Opt) map[string]any {
	t.Helper()

	server, body := captureRequestBody(t)
	cfg := &latest.ModelConfig{
		Provider: "openai",
		Model:    model,
		BaseURL:  server.URL,
		TokenKey: "MY_TOKEN",
		// Force Chat Completions even for a model that would otherwise
		// auto-select the Responses API.
		ProviderOpts: map[string]any{"api_type": "openai_chatcompletions"},
	}
	env := environment.NewMapEnvProvider(map[string]string{"MY_TOKEN": "secret"})

	client, err := NewClient(t.Context(), cfg, env, opts...)
	require.NoError(t, err)

	stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hi"}}, toolList)
	require.NoError(t, err)
	defer stream.Close()
	drainReasoningTestStream(t, stream)

	var req map[string]any
	require.NoError(t, json.Unmarshal(body(), &req))
	return req
}

// TestChatCompletions_DropsReasoningEffortWithTools is the regression test
// for docker/docker-agent#4162: gpt-5.4+ (and every gpt-5.6+/gpt-6+
// generation, see [modelinfo.OpenAIRejectsToolsWithReasoningEffort])
// rejects an explicit reasoning_effort alongside function tools on Chat
// Completions (verified live against api.openai.com). Forcing such a model
// onto Chat Completions (api_type override) with tools and a
// thinking_budget must drop the field rather than send a value guaranteed
// to 400, and must warn once so the drop is diagnosable.
func TestChatCompletions_DropsReasoningEffortWithTools(t *testing.T) {
	// Not parallel: it swaps the process-global default logger.
	var buf concurrent.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	req := chatCompletionsReasoningEffortField(t, "gpt-6-astra", toolsForReasoningTest())
	assert.NotContains(t, req, "reasoning_effort", "reasoning_effort must be dropped when tools force a model rejecting it onto Chat Completions")

	logs := buf.String()
	assert.Contains(t, logs, "dropping reasoning_effort")
	assert.Contains(t, logs, "gpt-6-astra")
}

// TestChatCompletions_DropsReasoningEffortWithTools_GPT54Boundary exercises
// the exact minor-version boundary where OpenAI started rejecting an
// explicit reasoning_effort with tools (gpt-5.4), one generation before the
// gpt-5.6/gpt-6 "no combination works" case.
func TestChatCompletions_DropsReasoningEffortWithTools_GPT54Boundary(t *testing.T) {
	t.Parallel()

	req := chatCompletionsReasoningEffortField(t, "gpt-5.4", toolsForReasoningTest())
	assert.NotContains(t, req, "reasoning_effort")
	require.Contains(t, req, "tools")
}

// TestChatCompletions_KeepsReasoningEffortWithoutTools is the control case:
// with no tools in the request, the existing thinking_budget behavior is
// unaffected by the new tools-present gate.
func TestChatCompletions_KeepsReasoningEffortWithoutTools(t *testing.T) {
	t.Parallel()

	req := chatCompletionsReasoningEffortField(t, "gpt-6-astra", nil)
	assert.Equal(t, "high", req["reasoning_effort"])
}

// TestChatCompletions_KeepsReasoningEffortWithTools_BelowGPT54 covers the
// OpenAI generations below the gpt-5.4 boundary (bare gpt-5, and the
// o-series, which is not a "gpt-" id at all): live-verified, these still
// accept an explicit reasoning_effort together with tools on Chat
// Completions, so the new gate must not fire for them and change existing
// behavior.
func TestChatCompletions_KeepsReasoningEffortWithTools_BelowGPT54(t *testing.T) {
	t.Parallel()

	for _, model := range []string{"gpt-5", "gpt-5.2", "o3-mini"} {
		t.Run(model, func(t *testing.T) {
			t.Parallel()
			req := chatCompletionsReasoningEffortField(t, model, toolsForReasoningTest())
			assert.Equal(t, "high", req["reasoning_effort"], "gate must not fire below the gpt-5.4 boundary")
		})
	}
}

// TestChatCompletions_NoWarnWhenNothingWasGoingToBeSent covers the ordering
// gap flagged in review: when tools force a rejecting model onto Chat
// Completions but no thinking_budget is configured and NoThinking() is not
// set, there was never a reasoning_effort about to be sent, so nothing was
// "dropped" — the WARN must not fire (it would be misleading, and would
// repeat on every turn of a long tool-using conversation).
func TestChatCompletions_NoWarnWhenNothingWasGoingToBeSent(t *testing.T) {
	// Not parallel: it swaps the process-global default logger.
	var buf concurrent.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	req := chatCompletionsRequest(t, "gpt-6-astra", toolsForReasoningTest())
	assert.NotContains(t, req, "reasoning_effort")
	assert.Empty(t, buf.String(), "nothing was going to be sent, so there is nothing to warn about")
}

// TestChatCompletions_DropsReasoningEffortWithTools_OverridesNoThinking
// exercises the branch ordering explicitly: the tools+reject case must take
// priority over NoThinking() too, not just over an explicit ThinkingBudget.
func TestChatCompletions_DropsReasoningEffortWithTools_OverridesNoThinking(t *testing.T) {
	// Not parallel: it swaps the process-global default logger.
	var buf concurrent.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	req := chatCompletionsRequest(t, "gpt-6-astra", toolsForReasoningTest(), options.WithNoThinking())
	assert.NotContains(t, req, "reasoning_effort", "the tools+reject gate must win over NoThinking(), not just over an explicit ThinkingBudget")
	assert.Contains(t, buf.String(), "dropping reasoning_effort", "NoThinking() was about to send an effort, so the drop must be reported")
}
