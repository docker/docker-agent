package openai

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/concurrent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelsdev"
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

func TestChatCompletions_GeminiThinkingBudget(t *testing.T) {
	// Not parallel: swaps the process-global default logger.
	var buf concurrent.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	geminiStore := modelsdev.NewDatabaseStore(&modelsdev.Database{
		Providers: map[string]modelsdev.Provider{
			"vercel": {Models: map[string]modelsdev.Model{
				"google/gemini-4-custom":        {Reasoning: false},
				"google/gemini-3.1-flash-image": {Reasoning: true},
			}},
		},
	})

	tests := []struct {
		name          string
		provider      string
		model         string
		maxTokens     *int64
		budget        *latest.ThinkingBudget
		opts          []options.Opt
		wantEffort    string
		wantMaxTokens int64
		wantOmitMax   bool
		wantWarn      bool
	}{
		{
			name:       "vercel gemini-4-neon xhigh",
			provider:   "vercel",
			model:      "google/gemini-4-neon",
			budget:     &latest.ThinkingBudget{Effort: "xhigh"},
			wantEffort: "xhigh",
		},
		{
			name:       "openrouter gemini-4-carbon clamps xhigh to high",
			provider:   "openrouter",
			model:      "google/gemini-4-carbon",
			budget:     &latest.ThinkingBudget{Effort: "xhigh"},
			wantEffort: "high",
		},
		{
			name:     "vercel gemini-4-neon adaptive omits reasoning_effort without warning",
			provider: "vercel",
			model:    "google/gemini-4-neon",
			budget:   &latest.ThinkingBudget{Effort: "adaptive"},
		},
		{
			name:       "vercel gemini-4-neon adaptive/low sends low",
			provider:   "vercel",
			model:      "google/gemini-4-neon",
			budget:     &latest.ThinkingBudget{Effort: "adaptive/low"},
			wantEffort: "low",
		},
		{
			name:       "vercel gemini-4-neon NoThinking sends low",
			provider:   "vercel",
			model:      "google/gemini-4-neon",
			opts:       []options.Opt{options.WithNoThinking()},
			wantEffort: "low",
		},
		{
			name:        "vercel gemini-4-neon NoThinking with zero MaxTokens does not apply floor",
			provider:    "vercel",
			model:       "google/gemini-4-neon",
			maxTokens:   new(int64(0)),
			opts:        []options.Opt{options.WithNoThinking()},
			wantEffort:  "low",
			wantOmitMax: true,
		},
		{
			name:          "vercel gemini-4-neon NoThinking with small MaxTokens raises to floor",
			provider:      "vercel",
			model:         "google/gemini-4-neon",
			maxTokens:     new(int64(50)),
			opts:          []options.Opt{options.WithNoThinking()},
			wantEffort:    "low",
			wantMaxTokens: noThinkingMinOutputTokens,
		},
		{
			name:     "vercel gemini-3-pro-image NoThinking omits reasoning_effort without warning",
			provider: "vercel",
			model:    "google/gemini-3-pro-image",
			opts:     []options.Opt{options.WithNoThinking()},
		},
		{
			name:     "vercel gemini-3-pro-image thinking_budget warns and omits reasoning_effort",
			provider: "vercel",
			model:    "google/gemini-3-pro-image",
			budget:   &latest.ThinkingBudget{Effort: "high"},
			wantWarn: true,
		},
		{
			name:       "vercel gemini-3.1-flash-image marked reasoning in store sends reasoning_effort",
			provider:   "vercel",
			model:      "google/gemini-3.1-flash-image",
			budget:     &latest.ThinkingBudget{Effort: "high"},
			opts:       []options.Opt{options.WithModelsDevStore(geminiStore)},
			wantEffort: "high",
		},
		{
			name:       "vercel gemini-3.1-flash-image marked reasoning in store sends low for NoThinking",
			provider:   "vercel",
			model:      "google/gemini-3.1-flash-image",
			opts:       []options.Opt{options.WithNoThinking(), options.WithModelsDevStore(geminiStore)},
			wantEffort: "low",
		},
		{
			name:     "vercel gemini-4-custom marked non-reasoning in store warns and omits reasoning_effort",
			provider: "vercel",
			model:    "google/gemini-4-custom",
			budget:   &latest.ThinkingBudget{Effort: "high"},
			opts:     []options.Opt{options.WithModelsDevStore(geminiStore)},
			wantWarn: true,
		},
		{
			name:     "openai-qualified gemini-4-hydrogen warns and ignores thinking_budget",
			provider: "openai",
			model:    "openai/gemini-4-hydrogen",
			budget:   &latest.ThinkingBudget{Effort: "high"},
			wantWarn: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf.Reset()
			server, body := captureRequestBody(t)
			cfg := &latest.ModelConfig{
				Provider:       tt.provider,
				Model:          tt.model,
				BaseURL:        server.URL,
				TokenKey:       "MY_TOKEN",
				MaxTokens:      tt.maxTokens,
				ThinkingBudget: tt.budget,
			}
			env := environment.NewMapEnvProvider(map[string]string{"MY_TOKEN": "secret"})

			client, err := NewClient(t.Context(), cfg, env, tt.opts...)
			require.NoError(t, err)

			stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hi"}}, nil)
			require.NoError(t, err)
			drainReasoningTestStream(t, stream)
			stream.Close()

			var req map[string]any
			require.NoError(t, json.Unmarshal(body(), &req))
			if tt.wantEffort != "" {
				assert.Equal(t, tt.wantEffort, req["reasoning_effort"])
			} else {
				assert.NotContains(t, req, "reasoning_effort")
			}

			if tt.wantMaxTokens != 0 {
				assert.EqualValues(t, tt.wantMaxTokens, req["max_tokens"])
			}
			if tt.wantOmitMax {
				assert.NotContains(t, req, "max_tokens")
			}

			if tt.wantWarn {
				assert.Contains(t, buf.String(), "does not support a reasoning effort parameter")
			} else {
				assert.NotContains(t, buf.String(), "does not support a reasoning effort parameter")
			}
		})
	}
}

func TestChatCompletions_GeminiDefaultSkipsModelsDevLookup(t *testing.T) {
	t.Parallel()

	var fetches atomic.Int32
	store, err := modelsdev.NewStore(
		modelsdev.WithCache(filepath.Join(t.TempDir(), "models.json")),
		modelsdev.WithFetcher(func(context.Context, string) (*modelsdev.Database, string, error) {
			fetches.Add(1)
			return &modelsdev.Database{}, "", nil
		}),
	)
	require.NoError(t, err)

	env := environment.NewMapEnvProvider(map[string]string{"MY_TOKEN": "secret"})

	t.Run("no override skips store lookup", func(t *testing.T) {
		server, body := captureRequestBody(t)
		cfg := &latest.ModelConfig{
			Provider:     "vercel",
			Model:        "google/gemini-4-neon",
			BaseURL:      server.URL,
			TokenKey:     "MY_TOKEN",
			Capabilities: &latest.CapabilitiesConfig{},
		}

		client, err := NewClient(t.Context(), cfg, env, options.WithModelsDevStore(store))
		require.NoError(t, err)

		stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hi"}}, nil)
		require.NoError(t, err)
		drainReasoningTestStream(t, stream)
		stream.Close()

		var req map[string]any
		require.NoError(t, json.Unmarshal(body(), &req))
		assert.NotContains(t, req, "reasoning_effort")
		assert.Zero(t, fetches.Load())
	})

	t.Run("configured thinking_budget queries store", func(t *testing.T) {
		server, body := captureRequestBody(t)
		cfg := &latest.ModelConfig{
			Provider:       "vercel",
			Model:          "google/gemini-4-neon",
			BaseURL:        server.URL,
			TokenKey:       "MY_TOKEN",
			Capabilities:   &latest.CapabilitiesConfig{},
			ThinkingBudget: &latest.ThinkingBudget{Effort: "high"},
		}

		client, err := NewClient(t.Context(), cfg, env, options.WithModelsDevStore(store))
		require.NoError(t, err)

		stream, err := client.CreateChatCompletionStream(t.Context(), []chat.Message{{Role: chat.MessageRoleUser, Content: "hi"}}, nil)
		require.NoError(t, err)
		drainReasoningTestStream(t, stream)
		stream.Close()

		var req map[string]any
		require.NoError(t, json.Unmarshal(body(), &req))
		assert.Equal(t, "high", req["reasoning_effort"])
		assert.Equal(t, int32(1), fetches.Load())
	})
}
