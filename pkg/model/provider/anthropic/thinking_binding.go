package anthropic

import (
	"errors"
	"slices"

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

func (c *Client) applyThinkingBinding(params *MessageRequest) {
	behavior := c.thinkingBindingBehavior()
	if behavior == "" {
		return
	}
	if params.Thinking != nil && (params.Thinking.Type == "adaptive" || params.Thinking.Type == "enabled") {
		params.Thinking.BlockBinding = &ThinkingBlockBindingRef{PrefixMismatchBehavior: behavior}
	}
	params.Betas = append(params.Betas, AnthropicBetaThinkingBindingControls2026_08_01)
}

func thinkingTransformations(entries []InputTransformation) []string {
	var result []string
	for _, entry := range entries {
		if entry.Type == "thinking_dropped" {
			result = append(result, entry.Path+": "+entry.Reason)
		}
	}
	return result
}
