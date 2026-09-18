package v15

// HasCustomBaseURL reports whether m targets a user-supplied endpoint: a
// base_url set directly on the model or inherited from a custom provider
// definition. It must be evaluated on the raw config — after provider
// defaults are applied a built-in alias default base URL is indistinguishable
// from a custom one.
//
// A custom base_url implies bypass_models_gateway: such endpoints are never
// routed through a configured models gateway.
func HasCustomBaseURL(m ModelConfig, providers map[string]ProviderConfig) bool {
	if m.BaseURL != "" {
		return true
	}
	p, exists := providers[m.Provider]
	return exists && p.BaseURL != ""
}
