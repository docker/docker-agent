package anthropic

import (
	"errors"
	"slices"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/docker/docker-agent/pkg/config/latest"
)

func validateThinkingOptions(cfg *latest.ModelConfig) error {
	if err := validateFallbackSpec(cfg); err != nil {
		return err
	}
	if err := validateThinkingDisplay(cfg); err != nil {
		return err
	}
	if raw, ok := cfg.ProviderOpts["thinking_prefix_mismatch"]; ok {
		value, ok := raw.(string)
		if !ok || (value != "error" && value != "drop_block") {
			return errors.New("anthropic: thinking_prefix_mismatch must be error or drop_block")
		}
	}
	return nil
}

// Prefix edits are normal in the runtime (hooks, trimming, compaction). Only
// invalidated thinking is dropped; complete unmodified blocks are still replayed.
func (c *Client) thinkingBindingBehavior() string {
	if value, ok := c.ModelConfig.ProviderOpts["thinking_prefix_mismatch"].(string); ok {
		return value
	}
	if slices.ContainsFunc(append([]string{c.ModelConfig.Model}, fallbackTargets(c.ModelConfig.ProviderOpts)...), checksThinkingPrefix) {
		return "drop_block"
	}
	return ""
}

func (c *Client) applyThinkingBinding(params *anthropic.BetaMessageNewParams) {
	behavior := c.thinkingBindingBehavior()
	if behavior == "" {
		return
	}
	binding := anthropic.BetaThinkingBlockBindingParam{PrefixMismatchBehavior: anthropic.BetaThinkingPrefixMismatchBehavior(behavior)}
	switch {
	case params.Thinking.OfAdaptive != nil:
		params.Thinking.OfAdaptive.BlockBinding = binding
	case params.Thinking.OfEnabled != nil:
		params.Thinking.OfEnabled.BlockBinding = binding
	default:
		// Older models may have thinking omitted while a fallback checks prefixes.
		// Do not turn thinking on just to send an optional binding control.
	}
	params.Betas = append(params.Betas, anthropic.AnthropicBetaThinkingBindingControls2026_08_01)
}

func thinkingTransformations(entries []anthropic.BetaInputTransformationUnion) []string {
	var result []string
	for _, entry := range entries {
		if entry.Type == "thinking_dropped" {
			result = append(result, entry.Path+": "+entry.Reason)
		}
	}
	return result
}
