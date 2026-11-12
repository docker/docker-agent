//go:build !docker_agent_no_hook_templates

package hooks

import (
	"encoding/json"
	"fmt"
	"strings"
	"text/template"
)

// parsePrompt compiles a model-hook prompt as a [text/template] over [Input].
func parsePrompt(prompt string) (func(*Input) (string, error), error) {
	tpl, err := template.New("hook-prompt").Funcs(promptFuncs).Parse(prompt)
	if err != nil {
		return nil, fmt.Errorf("parse prompt template: %w", err)
	}
	return func(in *Input) (string, error) {
		var sb strings.Builder
		if err := tpl.Execute(&sb, in); err != nil {
			return "", err
		}
		return sb.String(), nil
	}, nil
}

// promptFuncs is the [text/template] FuncMap exposed to model-hook
// prompts. Kept small on purpose: anything more elaborate belongs in
// the prompt itself.
var promptFuncs = template.FuncMap{
	// toJSON renders any value as compact JSON. Useful for embedding
	// .ToolInput / .ToolResponse in the user prompt without writing
	// custom Go-template formatting.
	"toJSON": func(v any) (string, error) {
		b, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return string(b), nil
	},
	// truncate caps a string at n bytes, appending "…" when cut. A
	// safety net for prompts that interpolate large tool inputs.
	"truncate": func(n int, s string) string {
		if n <= 0 || len(s) <= n {
			return s
		}
		return s[:n] + "…"
	},
}
