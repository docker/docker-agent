package openai

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/concurrent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

// TestChatCompletions_WarnsOnceWhenThinkingBudgetIgnored is the regression
// test for docker/docker-agent#4162: configuring thinking_budget on a model
// that does not accept a reasoning-effort parameter used to be silently
// dropped. It must now warn, and only once per client even across several
// requests.
func TestChatCompletions_WarnsOnceWhenThinkingBudgetIgnored(t *testing.T) {
	// Not parallel: it swaps the process-global default logger.
	var buf concurrent.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	server, _ := captureRequestBody(t)
	cfg := &latest.ModelConfig{
		Provider: "openai",
		// gpt-4o does not accept reasoning_effort at all.
		Model:          "gpt-4o",
		BaseURL:        server.URL,
		TokenKey:       "MY_TOKEN",
		ThinkingBudget: &latest.ThinkingBudget{Effort: "high"},
	}
	env := environment.NewMapEnvProvider(map[string]string{"MY_TOKEN": "secret"})

	client, err := NewClient(t.Context(), cfg, env)
	require.NoError(t, err)

	for range 2 {
		stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hi"}}, nil)
		require.NoError(t, err)
		drainReasoningTestStream(t, stream)
		stream.Close()
	}

	logs := buf.String()
	assert.Contains(t, logs, "thinking_budget is configured but this model does not support")
	assert.Contains(t, logs, "gpt-4o")
	assert.Equal(t, 1, strings.Count(logs, "does not support a reasoning effort parameter"), "the warning must fire at most once per client")
}

// TestResponsesAPI_WarnsOnceWhenThinkingBudgetIgnored is the Responses-API
// sibling: gpt-4.1 auto-selects the Responses API but does not accept a
// reasoning-effort parameter either.
func TestResponsesAPI_WarnsOnceWhenThinkingBudgetIgnored(t *testing.T) {
	// Not parallel: it swaps the process-global default logger.
	var buf concurrent.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	server, _ := captureResponsesRequestBody(t)
	cfg := &latest.ModelConfig{
		Provider:       "openai",
		Model:          "gpt-4.1",
		BaseURL:        server.URL,
		TokenKey:       "MY_TOKEN",
		ThinkingBudget: &latest.ThinkingBudget{Effort: "high"},
		ProviderOpts:   map[string]any{"api_type": "openai_responses"},
	}
	env := environment.NewMapEnvProvider(map[string]string{"MY_TOKEN": "secret"})

	client, err := NewClient(t.Context(), cfg, env)
	require.NoError(t, err)

	stream, err := client.CreateResponseStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hi"}}, nil)
	require.NoError(t, err)
	drainReasoningTestStream(t, stream)
	stream.Close()

	logs := buf.String()
	assert.Contains(t, logs, "thinking_budget is configured but this model does not support")
	assert.Contains(t, logs, "gpt-4.1")
}

// TestChatCompletions_NoWarnWhenThinkingBudgetDisabled ensures a disabled
// thinking_budget (thinking_budget: none/0) never triggers the ignored-budget
// warning: there is no configured effort to report as dropped.
func TestChatCompletions_NoWarnWhenThinkingBudgetDisabled(t *testing.T) {
	// Not parallel: it swaps the process-global default logger.
	var buf concurrent.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	server, _ := captureRequestBody(t)
	cfg := &latest.ModelConfig{
		Provider:       "openai",
		Model:          "gpt-4o",
		BaseURL:        server.URL,
		TokenKey:       "MY_TOKEN",
		ThinkingBudget: &latest.ThinkingBudget{Effort: "none"},
	}
	env := environment.NewMapEnvProvider(map[string]string{"MY_TOKEN": "secret"})

	client, err := NewClient(t.Context(), cfg, env)
	require.NoError(t, err)

	stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hi"}}, nil)
	require.NoError(t, err)
	drainReasoningTestStream(t, stream)
	stream.Close()

	assert.NotContains(t, buf.String(), "thinking_budget is configured")
}
