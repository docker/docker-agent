package latest

import (
	"fmt"
	"strings"
)

// ParseModelRef parses an inline "provider/model" reference into a
// ModelConfig. It returns an error when the string does not contain
// exactly one "/" separator or when either part is empty.
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
