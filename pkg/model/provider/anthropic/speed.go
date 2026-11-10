package anthropic

import (
	"errors"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/docker/docker-agent/pkg/config/latest"
)

const (
	speedOpt      = "speed"
	speedFast     = "fast"
	speedStandard = "standard"
)

func validateSpeed(cfg *latest.ModelConfig) error {
	raw, ok := cfg.ProviderOpts[speedOpt]
	if !ok {
		return nil
	}
	if value, ok := raw.(string); ok && (value == "" || value == speedFast || value == speedStandard) {
		return nil
	}
	return errors.New("anthropic: speed must be fast or standard")
}

func fastModeEnabled(opts map[string]any) bool {
	return opts[speedOpt] == speedFast
}

// The beta header is sent for every model: Opus 5 and Opus 4.8 require it and
// newer models ignore it, so no model allow-list is needed.
func applySpeed(params *anthropic.BetaMessageNewParams, opts map[string]any) {
	if !fastModeEnabled(opts) {
		return
	}
	params.Speed = anthropic.BetaMessageNewParamsSpeedFast
	params.Betas = append(params.Betas, anthropic.AnthropicBetaFastMode2026_02_01)
}

// servedTier only reports fast: standard requests keep an empty tier so their
// stored usage and cost are unchanged.
func servedTier(speed anthropic.BetaUsageSpeed) string {
	if speed == anthropic.BetaUsageSpeedFast {
		return speedFast
	}
	return ""
}
