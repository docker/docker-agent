//go:build docker_agent_no_js_expansion

package js

import (
	"cmp"
	"context"
	"regexp"
	"strings"

	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/tools"
)

// Expander is the engine-less ${...} expander selected by
// -tags docker_agent_no_js_expansion. It resolves ${env.NAME} and bound
// ${name} placeholders and leaves any other expression untouched: no
// JavaScript is evaluated and no tool is called. The tag drops goja, whose
// reflective method lookups stop the linker from pruning unused methods;
// size-sensitive embedders such as WASM builds use it.
type Expander struct {
	env environment.Provider
}

// NewJsExpander creates an Expander resolving ${env.NAME} from env.
func NewJsExpander(env environment.Provider) *Expander {
	return &Expander{env: env}
}

// NewEvaluator creates an Expander for command evaluation. Without the
// engine the tools cannot be called: ${tool(...)} expressions stay as text.
func NewEvaluator([]tools.Tool) *Expander {
	return &Expander{}
}

// reference matches an expression that is just env.NAME or NAME.
var reference = regexp.MustCompile(`^\s*(env\.)?([A-Za-z_][A-Za-z0-9_]*)\s*$`)

// Evaluate resolves ${env.NAME} placeholders; args are not available.
func (exp *Expander) Evaluate(ctx context.Context, input string, _ []string) string {
	return exp.Expand(ctx, input, nil)
}

// Expand resolves ${env.NAME} from the environment and ${name} from values.
// As with the engine, an unset variable expands to the empty string and an
// unbound name is left as-is.
func (exp *Expander) Expand(ctx context.Context, text string, values map[string]string) string {
	if !strings.Contains(text, "${") {
		return text
	}
	return expandReferences(ctx, exp.env, text, values)
}

// ExpandMap expands all values of the given map.
func (exp *Expander) ExpandMap(ctx context.Context, kv map[string]string) map[string]string {
	if kv == nil {
		return nil
	}
	env := exp.env
	expanded := make(map[string]string, len(kv))
	for k, v := range kv {
		expanded[k] = expandReferences(ctx, env, v, nil)
	}
	return expanded
}

// ExpandCommands expands the text fields of all commands.
func (exp *Expander) ExpandCommands(ctx context.Context, cmds types.Commands) types.Commands {
	if cmds == nil {
		return nil
	}
	env := exp.env
	expanded := make(types.Commands, len(cmds))
	for k, cmd := range cmds {
		cmd.Description = expandReferences(ctx, env, cmd.Description, nil)
		cmd.Instruction = expandReferences(ctx, env, cmd.Instruction, nil)
		cmd.URL = expandReferences(ctx, env, cmd.URL, nil)
		expanded[k] = cmd
	}
	return expanded
}

// ExpandMapFunc resolves ${objName.NAME} and ${objName["NAME"]} in map
// values through lookup. Each value is optionally preprocessed first.
func ExpandMapFunc(values map[string]string, objName string, lookup, preprocess func(string) string) map[string]string {
	member := regexp.MustCompile(`^\s*` + regexp.QuoteMeta(objName) + `(?:\.([A-Za-z_][A-Za-z0-9_]*)|\[\s*"([^"]*)"\s*\]|\[\s*'([^']*)'\s*\])\s*$`)

	resolved := make(map[string]string, len(values))
	for k, v := range values {
		if preprocess != nil {
			v = preprocess(v)
		}
		resolved[k] = expand(v, func(expr string) (string, bool) {
			m := member.FindStringSubmatch(expr)
			if m == nil {
				return "", false
			}
			return lookup(cmp.Or(m[1], m[2], m[3])), true
		})
	}
	return resolved
}

func expandReferences(ctx context.Context, env environment.Provider, text string, values map[string]string) string {
	return expand(text, func(expr string) (string, bool) {
		m := reference.FindStringSubmatch(expr)
		switch {
		case m == nil:
			return "", false
		case m[1] == "":
			v, ok := values[m[2]]
			return v, ok
		case env == nil:
			return "", false
		}
		v, _ := env.Get(ctx, m[2])
		return v, true
	})
}

// expand replaces each top-level ${...} expression of text that resolve
// accepts. The others are kept whole, placeholders nested in them included,
// as the engine keeps an expression it fails to evaluate.
func expand(text string, resolve func(expr string) (string, bool)) string {
	var result strings.Builder
	for i := 0; i < len(text); {
		idx := strings.Index(text[i:], "${")
		if idx < 0 {
			result.WriteString(text[i:])
			break
		}
		start := i + idx
		result.WriteString(text[i:start])

		end := findClosingBrace(text, start+2)
		if end < 0 {
			result.WriteString(text[start:])
			break
		}
		if v, ok := resolve(text[start+2 : end]); ok {
			result.WriteString(v)
		} else {
			result.WriteString(text[start : end+1])
		}
		i = end + 1
	}
	return result.String()
}
