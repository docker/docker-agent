package anthropic

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/modelinfo"
)

// Valid values for the `thinking_display` provider option.
const (
	thinkingDisplaySummarized = "summarized"
	thinkingDisplayOmitted    = "omitted"
	thinkingDisplayDisplay    = "display"
)

// adjustMaxTokensForThinking checks if max_tokens needs adjustment for thinking_budget.
// Anthropic's max_tokens represents the combined budget for thinking + output tokens.
// Returns the adjusted maxTokens value and an error if user-set max_tokens is too low.
//
// Only fixed token budgets need adjustment. Adaptive and effort-based budgets
// don't need it since the model manages its own thinking allocation.
func (c *Client) adjustMaxTokensForThinking(maxTokens int64) (int64, error) {
	if c.ModelConfig.ThinkingBudget == nil {
		return maxTokens, nil
	}
	// Adaptive and effort-based budgets: no token adjustment needed.
	if _, ok := anthropicThinkingEffort(c.ModelConfig.ThinkingBudget); ok {
		return maxTokens, nil
	}
	// Models that require adaptive thinking will have their token budget coerced
	// to adaptive at request time, so no adjustment is needed here either.
	if modelinfo.RejectsTokenThinking(c.ModelConfig.Model) {
		return maxTokens, nil
	}

	thinkingTokens := int64(c.ModelConfig.ThinkingBudget.Tokens)
	if thinkingTokens <= 0 {
		return maxTokens, nil
	}

	minRequired := thinkingTokens + 1024 // configured thinking budget + minimum output buffer

	if maxTokens <= thinkingTokens {
		userSetMaxTokens := c.ModelConfig.MaxTokens != nil
		if userSetMaxTokens {
			// User explicitly set max_tokens too low - return error
			slog.Error("Anthropic: max_tokens must be greater than thinking_budget",
				"max_tokens", maxTokens,
				"thinking_budget", thinkingTokens)
			return 0, fmt.Errorf("anthropic: max_tokens (%d) must be greater than thinking_budget (%d); increase max_tokens to at least %d",
				maxTokens, thinkingTokens, minRequired)
		}
		// Auto-adjust when user didn't set max_tokens
		slog.Info("Anthropic: auto-adjusting max_tokens to accommodate thinking_budget",
			"original_max_tokens", maxTokens,
			"thinking_budget", thinkingTokens,
			"new_max_tokens", minRequired)
		// return the configured thinking budget + 8192 because that's the default
		// max_tokens value for anthropic models when unspecified by the user
		return thinkingTokens + 8192, nil
	}

	return maxTokens, nil
}

// interleavedThinkingEnabled returns false unless explicitly enabled via
// models:provider_opts:interleaved_thinking: true
func (c *Client) interleavedThinkingEnabled() bool {
	if c == nil || len(c.ModelConfig.ProviderOpts) == 0 {
		return false
	}
	v, ok := c.ModelConfig.ProviderOpts["interleaved_thinking"]
	if !ok {
		return false
	}
	switch t := v.(type) {
	case bool:
		return t
	case string:
		s := strings.TrimSpace(strings.ToLower(t))
		return s != "false" && s != "0" && s != "no"
	case int:
		return t != 0
	case int64:
		return t != 0
	case float64:
		return t != 0
	default:
		return false
	}
}

// validThinkingTokens validates that the token budget is within the
// acceptable range for Anthropic (>= 1024 and < maxTokens).
// Returns (tokens, true) if valid, or (0, false) with a warning log if not.
func validThinkingTokens(tokens, maxTokens int64) (int64, bool) {
	if tokens < 1024 {
		slog.Warn("Anthropic thinking_budget below minimum (1024), ignoring", "tokens", tokens)
		return 0, false
	}
	if tokens >= maxTokens {
		slog.Warn("Anthropic thinking_budget must be less than max_tokens, ignoring", "tokens", tokens, "max_tokens", maxTokens)
		return 0, false
	}
	return tokens, true
}

// coerceAdaptiveThinking returns an adaptive ThinkingBudget when the configured
// model rejects token-based thinking budgets but the user supplied one.
// Otherwise it returns the configured budget unchanged. It never mutates
// c.ModelConfig.ThinkingBudget.
func (c *Client) coerceAdaptiveThinking() *latest.ThinkingBudget {
	budget := c.ModelConfig.ThinkingBudget
	if budget == nil {
		return nil
	}
	if _, ok := anthropicThinkingEffort(budget); ok {
		return budget // already adaptive or effort-based.
	}
	// Only coerce a real, positive token budget. Disabled/zero/negative
	// budgets are passed through so downstream code keeps treating them as
	// "thinking off" instead of silently enabling adaptive thinking.
	if budget.IsDisabled() || budget.Tokens <= 0 {
		return budget
	}
	if !modelinfo.RejectsTokenThinking(c.ModelConfig.Model) {
		return budget
	}
	slog.Warn("Anthropic: model rejects token-based thinking budgets; switching to adaptive thinking",
		"model", c.ModelConfig.Model,
		"thinking_budget_tokens", budget.Tokens)
	return &latest.ThinkingBudget{Effort: "adaptive"}
}

// anthropicThinkingEffort returns the Anthropic API effort level for the given
// ThinkingBudget. It covers both explicit adaptive mode and string effort
// levels. Returns ("", false) when the budget uses token counts or is nil.
func anthropicThinkingEffort(b *latest.ThinkingBudget) (string, bool) {
	if b == nil {
		return "", false
	}
	if e, ok := b.AdaptiveEffort(); ok {
		return e, true
	}
	l, ok := b.EffortLevel()
	if !ok {
		return "", false
	}
	return effort.ForAnthropic(l)
}

// anthropicThinkingDisplay returns the validated `thinking_display` value
// from provider_opts, if set. Valid values are "summarized", "omitted", and
// "display".
//
// Claude Opus 4.7 hides thinking content by default ("omitted"). Set
// thinking_display: summarized (or thinking_display: display) in
// provider_opts to receive thinking blocks, or thinking_display: omitted to
// explicitly hide them.
//
// Returns ("", false) when not set or invalid.
func anthropicThinkingDisplay(opts map[string]any) (string, bool) {
	v, ok := opts["thinking_display"]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	if !ok {
		slog.Debug("provider_opts type mismatch, ignoring",
			"key", "thinking_display",
			"expected_type", "string",
			"actual_type", fmt.Sprintf("%T", v),
			"value", v)
		return "", false
	}
	switch strings.TrimSpace(strings.ToLower(s)) {
	case thinkingDisplaySummarized:
		return thinkingDisplaySummarized, true
	case thinkingDisplayOmitted:
		return thinkingDisplayOmitted, true
	case thinkingDisplayDisplay:
		return thinkingDisplayDisplay, true
	default:
		slog.Warn("Anthropic provider_opts: invalid thinking_display value, ignoring",
			"value", s,
			"valid_values", []string{thinkingDisplaySummarized, thinkingDisplayOmitted, thinkingDisplayDisplay})
		return "", false
	}
}

// applyThinkingConfig configures extended thinking on a standard MessageNewParams
// based on the model's ThinkingBudget and provider_opts.thinking_display.
// Returns true when thinking is enabled (i.e., temperature/top_p must not be set).
func (c *Client) applyThinkingConfig(params *anthropic.MessageNewParams, maxTokens int64) bool {
	budget := c.coerceAdaptiveThinking()
	if budget == nil {
		return false
	}
	display, _ := anthropicThinkingDisplay(c.ModelConfig.ProviderOpts)

	if effortStr, ok := anthropicThinkingEffort(budget); ok {
		adaptive := &anthropic.ThinkingConfigAdaptiveParam{}
		if display != "" {
			adaptive.Display = anthropic.ThinkingConfigAdaptiveDisplay(display)
		}
		params.Thinking = anthropic.ThinkingConfigParamUnion{OfAdaptive: adaptive}
		params.OutputConfig.Effort = anthropic.OutputConfigEffort(effortStr)
		slog.Debug("Anthropic API using adaptive thinking", "effort", effortStr, "display", display)
		return true
	}

	tokens, ok := validThinkingTokens(int64(budget.Tokens), maxTokens)
	if !ok {
		return false
	}
	params.Thinking = anthropic.ThinkingConfigParamOfEnabled(tokens)
	if display != "" && params.Thinking.OfEnabled != nil {
		params.Thinking.OfEnabled.Display = anthropic.ThinkingConfigEnabledDisplay(display)
	}
	slog.Debug("Anthropic API using thinking_budget", "budget_tokens", tokens, "display", display)
	return true
}

// applyBetaThinkingConfig configures extended thinking on a BetaMessageNewParams
// based on the model's ThinkingBudget and provider_opts.thinking_display.
func (c *Client) applyBetaThinkingConfig(params *anthropic.BetaMessageNewParams, maxTokens int64) {
	budget := c.coerceAdaptiveThinking()
	if budget == nil {
		return
	}
	display, _ := anthropicThinkingDisplay(c.ModelConfig.ProviderOpts)

	if effortStr, ok := anthropicThinkingEffort(budget); ok {
		adaptive := &anthropic.BetaThinkingConfigAdaptiveParam{}
		if display != "" {
			adaptive.Display = anthropic.BetaThinkingConfigAdaptiveDisplay(display)
		}
		params.Thinking = anthropic.BetaThinkingConfigParamUnion{OfAdaptive: adaptive}
		params.OutputConfig.Effort = anthropic.BetaOutputConfigEffort(effortStr)
		slog.Debug("Anthropic Beta API using adaptive thinking", "effort", effortStr, "display", display)
		return
	}

	tokens, ok := validThinkingTokens(int64(budget.Tokens), maxTokens)
	if !ok {
		return
	}
	params.Thinking = anthropic.BetaThinkingConfigParamOfEnabled(tokens)
	if display != "" && params.Thinking.OfEnabled != nil {
		params.Thinking.OfEnabled.Display = anthropic.BetaThinkingConfigEnabledDisplay(display)
	}
	slog.Debug("Anthropic Beta API using thinking_budget", "budget_tokens", tokens, "display", display)
}
