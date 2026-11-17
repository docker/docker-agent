package config

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/config/latest"
)

func TestWarnMaxTokens_ExceedsContextWindow(t *testing.T) {
	t.Parallel()

	maxTokens := int64(262144)
	cfg := &latest.Config{
		Providers: map[string]latest.ProviderConfig{
			"my_llm": {
				Provider:     "dmr",
				MaxTokens:    &maxTokens,
				ProviderOpts: map[string]any{"context_size": 262144},
			},
		},
	}

	out := captureWarnings(t, func(ctx context.Context, logger *slog.Logger) {
		warnMaxTokensVsContextWindow(ctx, logger, cfg)
	})

	assert.Contains(t, out, "provider my_llm")
	assert.Contains(t, out, "context window")
	assert.Contains(t, out, "3387")
}

// TestWarnMaxTokens_LargeValueWithoutContextWindow covers the reporter's exact
// config: max_tokens set to a context-window-sized value with no context_window,
// so the window is not discoverable at load time. The instructional warning is
// the only signal that reaches this user.
func TestWarnMaxTokens_LargeValueWithoutContextWindow(t *testing.T) {
	t.Parallel()

	maxTokens := int64(262144)
	cfg := &latest.Config{
		Providers: map[string]latest.ProviderConfig{
			"my_llm": {MaxTokens: &maxTokens},
		},
	}

	out := captureWarnings(t, func(ctx context.Context, logger *slog.Logger) {
		warnMaxTokensVsContextWindow(ctx, logger, cfg)
	})

	assert.Contains(t, out, "provider my_llm")
	assert.Contains(t, out, "context_window")
}

func TestWarnMaxTokens_ReasonableOutputCapIsSilent(t *testing.T) {
	t.Parallel()

	window := int64(262144)

	providerMax := int64(4096)
	modelMax := int64(8192)
	cfg := &latest.Config{
		Providers: map[string]latest.ProviderConfig{
			"my_llm": {MaxTokens: &providerMax},
		},
		Models: map[string]latest.ModelConfig{
			"main": {
				MaxTokens:     &modelMax,
				ContextWindow: &window,
			},
		},
	}

	out := captureWarnings(t, func(ctx context.Context, logger *slog.Logger) {
		warnMaxTokensVsContextWindow(ctx, logger, cfg)
	})

	assert.Empty(t, out, "a sensible output cap below the window must not warn")
}

func TestWarnMaxTokens_ModelExceedsContextWindow(t *testing.T) {
	t.Parallel()

	maxTokens := int64(200000)
	window := int64(131072)
	cfg := &latest.Config{
		Models: map[string]latest.ModelConfig{
			"main": {
				MaxTokens:     &maxTokens,
				ContextWindow: &window,
			},
		},
	}

	out := captureWarnings(t, func(ctx context.Context, logger *slog.Logger) {
		warnMaxTokensVsContextWindow(ctx, logger, cfg)
	})

	assert.Contains(t, out, "model main")
	assert.Contains(t, out, "3387")
}

// A model-level context_window is the declared window for the model entry, so an
// oversized max_tokens on that entry is flagged just like provider_opts.
func TestWarnMaxTokens_ModelLevelContextWindow(t *testing.T) {
	t.Parallel()

	maxTokens := int64(262144)
	window := int64(262144)
	cfg := &latest.Config{
		Models: map[string]latest.ModelConfig{
			"my_model": {MaxTokens: &maxTokens, ContextWindow: &window},
		},
	}

	out := captureWarnings(t, func(ctx context.Context, logger *slog.Logger) {
		warnMaxTokensVsContextWindow(ctx, logger, cfg)
	})

	assert.Contains(t, out, "model my_model")
	assert.Contains(t, out, "clamped")
}

// provider_opts.context_size is DMR-only, so it is not a window for a model on
// any other provider and an oversized max_tokens there must not be attributed
// to it.
func TestWarnMaxTokens_ProviderOptsIgnoredOutsideDMR(t *testing.T) {
	t.Parallel()

	maxTokens := int64(262144)
	cfg := &latest.Config{
		Models: map[string]latest.ModelConfig{
			"main": {Provider: "openai", MaxTokens: &maxTokens, ProviderOpts: map[string]any{"context_size": 262144}},
		},
	}

	out := captureWarnings(t, func(ctx context.Context, logger *slog.Logger) {
		warnMaxTokensVsContextWindow(ctx, logger, cfg)
	})

	assert.Contains(t, out, "context_window", "the unknown-window hint must point to context_window")
	assert.NotContains(t, out, "it is >= the context window", "the window must not be read from provider_opts outside DMR")
}
