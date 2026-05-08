package js

import (
	"context"
	"strings"

	"github.com/dop251/goja"

	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/environment"
)

// newVM creates a new Goja JavaScript runtime.
var newVM = goja.New

// Expander evaluates JavaScript expressions.
type Expander struct {
	env environment.Provider
}

// NewJsExpander creates a new Expander.
func NewJsExpander(env environment.Provider) *Expander {
	return &Expander{env: env}
}

func (exp *Expander) newVM(ctx context.Context) *goja.Runtime {
	vm := newVM()
	if exp.env != nil {
		_ = vm.Set("env", vm.NewDynamicObject(&dynamicLookup{
			vm:     vm,
			lookup: func(k string) string { v, _ := exp.env.Get(ctx, k); return v },
		}))
	}
	return vm
}

// Expand evaluates JavaScript template literals.
func (exp *Expander) Expand(ctx context.Context, text string, values map[string]string) string {
	if !strings.Contains(text, "${") {
		return text
	}

	vm := exp.newVM(ctx)
	for k, v := range values {
		_ = vm.Set(k, v)
	}

	return runExpansion(vm, text)
}

// ExpandMap evaluates JavaScript template literals in all values of the given map.
func (exp *Expander) ExpandMap(ctx context.Context, kv map[string]string) map[string]string {
	if kv == nil {
		return nil
	}

	vm := exp.newVM(ctx)

	expanded := make(map[string]string, len(kv))
	for k, v := range kv {
		expanded[k] = runExpansion(vm, v)
	}
	return expanded
}

// ExpandCommands evaluates JavaScript template literals in all command fields.
func (exp *Expander) ExpandCommands(ctx context.Context, cmds types.Commands) types.Commands {
	if cmds == nil {
		return nil
	}

	vm := exp.newVM(ctx)

	expanded := make(types.Commands, len(cmds))
	for k, cmd := range cmds {
		expanded[k] = types.Command{
			Description: runExpansion(vm, cmd.Description),
			Instruction: runExpansion(vm, cmd.Instruction),
		}
	}
	return expanded
}

// ExpandMapFunc evaluates JavaScript template literals in map values.
func ExpandMapFunc(values map[string]string, objName string, lookup, preprocess func(string) string) map[string]string {
	vm := newVM()
	if lookup != nil {
		_ = vm.Set(objName, vm.NewDynamicObject(&dynamicLookup{
			vm:     vm,
			lookup: lookup,
		}))
	}

	resolved := make(map[string]string, len(values))
	for k, v := range values {
		if preprocess != nil {
			v = preprocess(v)
		}
		resolved[k] = runExpansion(vm, v)
	}
	return resolved
}

// dynamicLookup implements goja.DynamicObject for lazy key-value access.
type dynamicLookup struct {
	vm     *goja.Runtime
	lookup func(string) string
}

func (d *dynamicLookup) Get(k string) goja.Value   { return d.vm.ToValue(d.lookup(k)) }
func (*dynamicLookup) Set(string, goja.Value) bool { return false }
func (*dynamicLookup) Has(string) bool             { return true }
func (*dynamicLookup) Delete(string) bool          { return true }
func (*dynamicLookup) Keys() []string              { return nil }

// runExpansion executes the template string using the provided Goja runtime.
func runExpansion(vm *goja.Runtime, text string) string {
	// Escape backslashes first, then backticks
	escaped := strings.ReplaceAll(text, "\\", "\\\\")
	escaped = strings.ReplaceAll(escaped, "`", "\\`")
	script := "`" + escaped + "`"

	v, err := vm.RunString(script)
	if err == nil {
		if v == nil || v.Export() == nil {
			return ""
		}
		return v.String()
	}

	// Full template failed — try each ${...} expression individually.
	return expandExpressions(vm, text)
}

// expandExpressions evaluates each ${...} expression in text individually,
// replacing successful ones with their result and leaving failed ones as-is.
func expandExpressions(vm *goja.Runtime, text string) string {
	var result strings.Builder
	i := 0
	for i < len(text) {
		// Look for ${
		idx := strings.Index(text[i:], "${")
		if idx < 0 {
			result.WriteString(text[i:])
			break
		}
		result.WriteString(text[i : i+idx])
		exprStart := i + idx

		// Find matching closing brace, accounting for nested braces and strings.
		end := findClosingBrace(text, exprStart+2)
		if end < 0 {
			// Unclosed expression — write the rest as-is.
			result.WriteString(text[exprStart:])
			break
		}

		expr := text[exprStart+2 : end] // content between ${ and }
		full := text[exprStart : end+1] // ${...} including delimiters

		v, err := vm.RunString(expr)
		switch {
		case err != nil:
			result.WriteString(full) // keep original
		case v == nil || goja.IsUndefined(v) || goja.IsNull(v):
			// Match JS template literal behavior: null/undefined become empty string.
		default:
			result.WriteString(v.String())
		}
		i = end + 1
	}
	return result.String()
}

// findClosingBrace returns the index of the closing '}' for a ${...} expression
// starting at pos (which points to the first character after "${").
// It handles nested braces, template literals, and quoted strings.
// Returns -1 if no matching brace is found.
func findClosingBrace(text string, pos int) int {
	depth := 1
	var quote byte
	for i := pos; i < len(text) && depth > 0; i++ {
		ch := text[i]
		if quote != 0 {
			if ch == '\\' && i+1 < len(text) {
				i++ // skip escaped char
				continue
			}
			if ch == quote {
				quote = 0
			}
			continue
		}
		switch ch {
		case '"', '\'', '`':
			quote = ch
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}
