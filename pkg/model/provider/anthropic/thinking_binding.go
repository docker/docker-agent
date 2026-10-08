package anthropic

import (
	"errors"
	"fmt"
	"slices"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/providerutil"
	"github.com/docker/docker-agent/pkg/modelinfo"
)

func validateThinkingOptions(cfg *latest.ModelConfig) error {
	fallbacks, _ := providerutil.GetProviderOptStringSlice(cfg.ProviderOpts, "fallbacks")
	if len(fallbacks) > 0 {
		for _, model := range append([]string{cfg.Model}, fallbacks...) {
			if modelinfo.IsClaudeHaiku55(model) {
				return fmt.Errorf("anthropic: model %q does not support server-side fallbacks; use docker-agent client-side routing or first_available instead", model)
			}
		}
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
	fallbacks, _ := providerutil.GetProviderOptStringSlice(c.ModelConfig.ProviderOpts, "fallbacks")
	if slices.ContainsFunc(append([]string{c.ModelConfig.Model}, fallbacks...), checksThinkingPrefix) {
		return "drop_block"
	}
	return ""
}

func (c *Client) applyThinkingBinding(params *anthropic.BetaMessageNewParams) {
	if modelinfo.IsClaudeHaiku55(c.ModelConfig.Model) && params.Thinking.OfAdaptive == nil {
		return
	}
	behavior := c.thinkingBindingBehavior()
	if behavior == "" {
		return
	}
	binding := anthropic.BetaThinkingBlockBindingParam{PrefixMismatchBehavior: anthropic.BetaThinkingPrefixMismatchBehavior(behavior)}
	switch {
	case params.Thinking.OfAdaptive != nil:
		params.Thinking.OfAdaptive.BlockBinding = binding
	case params.Thinking.OfEnabled != nil && !modelinfo.IsClaudeHaiku55(c.ModelConfig.Model):
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

func (c *Client) validateLastRole(role string) error {
	if modelinfo.IsClaudeHaiku55(c.ModelConfig.Model) && role == "assistant" {
		return errors.New("anthropic: Claude Haiku 5.5 rejects assistant prefill; end the request with a user turn or a tool_result")
	}
	return nil
}
