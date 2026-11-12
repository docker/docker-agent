package openai

import "log/slog"

// Leave tier validation to the API so new and provider-specific tiers pass through.
func serviceTier(opts map[string]any) string {
	value, ok := opts["service_tier"]
	if !ok {
		return ""
	}
	tier, ok := value.(string)
	if !ok {
		slog.Debug("OpenAI provider_opts: service_tier must be a string, ignoring", "value", value)
	}
	return tier
}
