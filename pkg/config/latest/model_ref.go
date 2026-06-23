package latest

import (
	"fmt"
	"strings"
)

// ParseModelRef parses an inline "provider/model" reference into a
// ModelConfig. It splits on the first "/", so the model portion may itself
// contain slashes (e.g. "dmr/ai/qwen3:latest" yields provider "dmr" and model
// "ai/qwen3:latest"). It returns an error when there is no "/" or when either
// part is empty.
//
//	cfg, err := ParseModelRef("openai/gpt-4o")
//	// cfg.Provider == "openai", cfg.Model == "gpt-4o"
func ParseModelRef(ref string) (ModelConfig, error) {
	providerName, model, ok := strings.Cut(ref, "/")
	if !ok || providerName == "" || model == "" {
		return ModelConfig{}, fmt.Errorf("invalid model reference %q: expected 'provider/model' format", ref)
	}
	return ModelConfig{Provider: providerName, Model: model}, nil
}
