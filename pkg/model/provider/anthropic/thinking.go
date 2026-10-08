package anthropic

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/model/provider/providerutil"
	"github.com/docker/docker-agent/pkg/modelinfo"
)

// Valid values for the `thinking_display` provider option.
const (
	thinkingDisplaySummarized = "summarized"
	thinkingDisplayOmitted    = "omitted"
	thinkingDisplayDisplay    = "display"
	thinkingDisplayUpdates    = "updates"
)

// noThinkingMinOutputTokens is the minimum output-token budget enforced when
// thinking is disabled but the caller imposed a tiny max_tokens cap. Anthropic
// adaptive-thinking models (e.g. Claude Opus 4.6+) always reason internally,
// and those hidden thinking tokens count against max_tokens. A small cap (title
// generation uses 20) can be entirely consumed by reasoning, leaving no visible
// output and producing an empty title. This mirrors the OpenAI provider's floor
// for reasoning models.
const noThinkingMinOutputTokens int64 = 256

// floorMaxTokensForNoThinking raises a tiny max_tokens cap to
// noThinkingMinOutputTokens when the caller disabled thinking (e.g. title or
// summary generation). The floor is scoped to the NoThinking path so it never
// overrides an explicit user-set cap during normal completions.
func (c *Client) floorMaxTokensForNoThinking(maxTokens int64) int64 {
	if c.ModelOptions.NoThinking() && maxTokens < noThinkingMinOutputTokens {
		return noThinkingMinOutputTokens
	}
	return maxTokens
}

// adjustMaxTokensForThinking checks if max_tokens needs adjustment for thinking_budget.
// Anthropic's max_tokens represents the combined budget for thinking + output tokens.
// Returns the adjusted maxTokens value and an error if user-set max_tokens is too low.
//
// It operates on the resolved budget (see [Client.resolveThinkingBudget]) so an
// effort level that falls back to a token budget on a non-adaptive model gets
// the same headroom as any other token budget. Only fixed token budgets need
// adjustment; adaptive and effort-based budgets are managed by the model itself.
func (c *Client) adjustMaxTokensForThinking(maxTokens int64) (int64, error) {
	budget := c.resolveThinkingBudget()
	if budget == nil {
		return maxTokens, nil
	}
	// Adaptive and effort-based budgets: no token adjustment needed; the model
	// manages its own thinking allocation within max_tokens.
	if _, ok := anthropicThinkingEffort(budget); ok {
		return maxTokens, nil
	}

	thinkingTokens := int64(budget.Tokens)
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

// resolveThinkingBudget returns the ThinkingBudget to actually send to the API,
// adapting the configured budget to what the target model accepts. It never
// mutates c.ModelConfig.ThinkingBudget.
//
// Two model-specific rewrites happen here, in opposite directions:
//   - A token budget on a model that rejects token-based thinking (Opus 4.6+)
//     becomes adaptive thinking.
//   - An effort or adaptive budget on a model that does not support adaptive
//     thinking (Haiku 4.5, Sonnet 4.5 and earlier, ...) becomes a token budget,
//     since `thinking.type=adaptive`/`output_config.effort` are rejected with a
//     400 on those models (issue #3362). This is the "regular thinking options"
//     path for effort levels set via the TUI Shift+Tab cycle.
//
// Disabled, zero, and negative budgets are passed through unchanged so
// downstream code keeps treating them as "thinking off".
func (c *Client) resolveThinkingBudget() *latest.ThinkingBudget {
	budget := c.ModelConfig.ThinkingBudget
	if modelinfo.IsClaudeHaiku55(c.ModelConfig.Model) && c.ModelOptions.NoThinking() {
		return &latest.ThinkingBudget{Effort: "none"}
	}
	if budget == nil {
		if checksThinkingPrefix(c.ModelConfig.Model) || c.progressUpdatesEnabled() {
			return &latest.ThinkingBudget{Effort: "adaptive"}
		}
		return nil
	}
	if budget.IsDisabled() {
		if requiresThinking(c.ModelConfig.Model) {
			return &latest.ThinkingBudget{Effort: "adaptive/low"}
		}
		return budget
	}

	if _, ok := anthropicThinkingEffort(budget); ok {
		// Effort or adaptive budget.
		if modelinfo.SupportsAdaptiveThinking(c.ModelConfig.Model) {
			return budget
		}
		tokens, ok := effortBudgetTokens(budget)
		if !ok {
			return budget
		}
		slog.Warn("Anthropic: model does not support adaptive thinking; using token-based thinking budget",
			"model", c.ModelConfig.Model,
			"effort", budget.Effort,
			"budget_tokens", tokens)
		return &latest.ThinkingBudget{Tokens: tokens}
	}

	// Token budget. Only coerce a real, positive value.
	if budget.Tokens <= 0 || (!modelinfo.RejectsTokenThinking(c.ModelConfig.Model) && !usesDefaultThinking(c.ModelConfig.Model)) {
		return budget
	}
	if modelinfo.IsClaudeHaiku55(c.ModelConfig.Model) {
		slog.Warn("Anthropic: exact thinking token ceilings cannot be retained; using adaptive medium", "model", c.ModelConfig.Model)
		return &latest.ThinkingBudget{Effort: "adaptive/medium"}
	}
	slog.Warn("Anthropic: model rejects token-based thinking budgets; switching to adaptive thinking", "model", c.ModelConfig.Model, "thinking_budget_tokens", budget.Tokens)
	return &latest.ThinkingBudget{Effort: "adaptive"}
}

func (c *Client) dropPriorThinking() bool {
	if !modelinfo.IsClaudeHaiku55(c.ModelConfig.Model) {
		return false
	}
	return c.ModelOptions.NoThinking() || c.ModelConfig.ThinkingBudget.IsDisabled()
}

// effortBudgetTokens maps an effort-based or adaptive ThinkingBudget onto a
// token budget, for models that only support token-based extended thinking.
// It covers both plain effort levels ("high") and adaptive forms ("adaptive",
// "adaptive/low"). Returns (0, false) for token-count or unrecognised budgets.
func effortBudgetTokens(b *latest.ThinkingBudget) (int, bool) {
	level, ok := b.AdaptiveEffort()
	if !ok {
		l, ok := b.EffortLevel()
		if !ok {
			return 0, false
		}
		return effort.BedrockTokens(l)
	}
	l, ok := effort.Parse(level)
	if !ok {
		return 0, false
	}
	return effort.BedrockTokens(l)
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
// "display" (the latter only on pre-4.6 models, enforced by
// [validateThinkingDisplay] at client construction).
//
// Claude Opus 4.7 hides thinking content by default ("omitted"). Set
// thinking_display: summarized in provider_opts to receive thinking blocks,
// or thinking_display: omitted to explicitly hide them.
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
	case thinkingDisplayUpdates:
		return thinkingDisplayUpdates, true
	default:
		slog.Warn("Anthropic provider_opts: invalid thinking_display value, ignoring",
			"value", s,
			"valid_values", []string{thinkingDisplaySummarized, thinkingDisplayOmitted, thinkingDisplayDisplay, thinkingDisplayUpdates})
		return "", false
	}
}

// validateThinkingDisplay rejects a thinking_display mode the target model
// does not accept. Models from the adaptive-thinking generation onward only
// accept "summarized" and "omitted"; sending "display" fails with an HTTP
// 400 (see [modelinfo.SupportsFullThinkingDisplay]). Server-side fallback
// models receive the exact same request shape, so they are validated too.
// Called at client construction so a bad config fails fast instead of on the
// first request.
func validateThinkingDisplay(cfg *latest.ModelConfig) error {
	display, ok := anthropicThinkingDisplay(cfg.ProviderOpts)
	if !ok || (display != thinkingDisplayDisplay && display != thinkingDisplayUpdates) {
		return nil
	}
	// Read fallbacks directly (not via fallbackModels) to avoid its
	// "enabling server-side fallbacks" debug log during validation.
	fallbacks, _ := providerutil.GetProviderOptStringSlice(cfg.ProviderOpts, "fallbacks")
	for _, model := range append([]string{cfg.Model}, fallbacks...) {
		if display == thinkingDisplayUpdates {
			if !supportsProgressUpdates(model) {
				return fmt.Errorf("anthropic: model %q does not support thinking_display: updates", model)
			}
			continue
		}
		if !modelinfo.SupportsFullThinkingDisplay(model) {
			return fmt.Errorf("anthropic: model %q does not support thinking_display: %q; use %q or %q",
				model, thinkingDisplayDisplay, thinkingDisplaySummarized, thinkingDisplayOmitted)
		}
	}
	return nil
}

// defaultAdaptiveDisplay returns the thinking display to request with
// adaptive thinking when the user did not configure one. Newer Claude models
// (Opus 4.7+, Fable 5) default to omitted server-side, which silently hides
// all reasoning from the UI — including when an effort level is set via the
// TUI (/effort, Shift+Tab). Requesting summarized keeps reasoning visible and
// matches the documented default of older models; thinking_display: omitted
// remains the explicit opt-out.
func defaultAdaptiveDisplay(display string) string {
	if display == "" {
		return thinkingDisplaySummarized
	}
	return display
}

// applyThinkingConfig configures extended thinking on a standard MessageNewParams
// based on the model's ThinkingBudget and provider_opts.thinking_display.
// Returns true when thinking is enabled (i.e., temperature/top_p must not be set).
func (c *Client) applyThinkingConfig(params *anthropic.MessageNewParams, maxTokens int64) bool {
	budget := c.resolveThinkingBudget()
	if budget == nil {
		return usesDefaultThinking(c.ModelConfig.Model)
	}
	if budget.IsDisabled() {
		params.Thinking = anthropic.ThinkingConfigParamUnion{OfDisabled: &anthropic.ThinkingConfigDisabledParam{}}
		return false
	}
	display, _ := anthropicThinkingDisplay(c.ModelConfig.ProviderOpts)

	if effortStr, ok := anthropicThinkingEffort(budget); ok {
		display = defaultAdaptiveDisplay(display)
		adaptive := &anthropic.ThinkingConfigAdaptiveParam{
			Display: anthropic.ThinkingConfigAdaptiveDisplay(display),
		}
		params.Thinking = anthropic.ThinkingConfigParamUnion{OfAdaptive: adaptive}
		if c.ModelConfig.ThinkingBudget != nil {
			params.OutputConfig.Effort = anthropic.OutputConfigEffort(effortStr)
		}
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
	budget := c.resolveThinkingBudget()
	if budget == nil {
		return
	}
	if budget.IsDisabled() {
		params.Thinking = anthropic.BetaThinkingConfigParamUnion{OfDisabled: &anthropic.BetaThinkingConfigDisabledParam{}}
		return
	}
	display, _ := anthropicThinkingDisplay(c.ModelConfig.ProviderOpts)
	if display == thinkingDisplayUpdates {
		params.Betas = append(params.Betas, anthropic.AnthropicBetaThinkingDisplayUpdates2026_08_18)
	}

	if effortStr, ok := anthropicThinkingEffort(budget); ok {
		display = defaultAdaptiveDisplay(display)
		adaptive := &anthropic.BetaThinkingConfigAdaptiveParam{
			Display: anthropic.BetaThinkingConfigAdaptiveDisplay(display),
		}
		params.Thinking = anthropic.BetaThinkingConfigParamUnion{OfAdaptive: adaptive}
		if c.ModelConfig.ThinkingBudget != nil {
			params.OutputConfig.Effort = anthropic.BetaOutputConfigEffort(effortStr)
		}
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

func (c *Client) progressUpdatesEnabled() bool {
	display, _ := anthropicThinkingDisplay(c.ModelConfig.ProviderOpts)
	return display == thinkingDisplayUpdates
}
