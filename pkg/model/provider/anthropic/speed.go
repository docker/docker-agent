package anthropic

import (
	"errors"

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

func applySpeed(params *MessageRequest, opts map[string]any) {
	if !fastModeEnabled(opts) {
		return
	}
	params.Speed = speedFast
	params.Betas = append(params.Betas, AnthropicBetaFastMode2026_02_01)
}

// servedTier only reports fast: standard requests keep an empty tier so their
// stored usage and cost are unchanged.
func servedTier(speed string) string {
	if speed == speedFast {
		return speedFast
	}
	return ""
}
