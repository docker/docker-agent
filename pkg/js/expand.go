package js

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/dop251/goja"

	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/tools"
)

// newVM creates a new Goja JavaScript runtime.
var newVM = goja.New

// Expander expands JavaScript template literals in strings.
// It can be configured with an environment provider for ${env.X} access
// and/or agent tools for ${tool({...})} calls.
type Expander struct {
	env   environment.Provider
	tools []tools.Tool
}

// NewJsExpander creates a new Expander with the given environment provider.
func NewJsExpander(env environment.Provider) *Expander {
	return &Expander{env: env}
}

// NewEvaluator creates a new Expander with the given tools (for command evaluation).
func NewEvaluator(agentTools []tools.Tool) *Expander {
	return &Expander{tools: agentTools}
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

// newVMWithBindings creates a new JS runtime with env and tools pre-bound.
func (exp *Expander) newVMWithBindings(ctx context.Context) *goja.Runtime {
	vm := newVM()

	if exp.env != nil {
		_ = vm.Set("env", vm.NewDynamicObject(&dynamicLookup{
			vm:     vm,
			lookup: func(k string) string { v, _ := exp.env.Get(ctx, k); return v },
		}))
	}

	for _, tool := range exp.tools {
		_ = vm.Set(tool.Name, createToolCaller(ctx, tool))
	}

	return vm
}

// Evaluate finds and evaluates ${...} JavaScript expressions in the input string.
// args are available as the 'args' array in JavaScript.
func (exp *Expander) Evaluate(ctx context.Context, input string, args []string) string {
	if !strings.Contains(input, "${") {
		return input
	}

	vm := exp.newVMWithBindings(ctx)
	if args == nil {
		args = []string{}
	}
	_ = vm.Set("args", args)

	slog.DebugContext(ctx, "Evaluating JS template", "input", input)

	return runExpansion(vm, input)
}

// Expand expands JavaScript template literals using the provided values map.
// The values are bound as top-level variables in the JS runtime alongside
// env and tools bindings.
func (exp *Expander) Expand(ctx context.Context, text string, values map[string]string) string {
	if !strings.Contains(text, "${") {
		return text
	}

	vm := exp.newVMWithBindings(ctx)
	for k, v := range values {
		_ = vm.Set(k, v)
	}

	return runExpansion(vm, text)
}

// ExpandMap expands JavaScript template literals in all values of the given map.
func (exp *Expander) ExpandMap(ctx context.Context, kv map[string]string) map[string]string {
	if kv == nil {
		return nil
	}

	vm := exp.newVMWithBindings(ctx)

	expanded := make(map[string]string, len(kv))
	for k, v := range kv {
		expanded[k] = runExpansion(vm, v)
	}
	return expanded
}

// ExpandCommands expands JavaScript template literals in all command fields.
func (exp *Expander) ExpandCommands(ctx context.Context, cmds types.Commands) types.Commands {
	if cmds == nil {
		return nil
	}

	vm := exp.newVMWithBindings(ctx)

	expanded := make(types.Commands, len(cmds))
	for k, cmd := range cmds {
		expanded[k] = types.Command{
			Description: runExpansion(vm, cmd.Description),
			Instruction: runExpansion(vm, cmd.Instruction),
		}
	}
	return expanded
}

// ExpandMapFunc expands JavaScript template literals in map values.
// It binds a dynamic object with the given name to the JS runtime,
// using lookup to resolve property accesses. Each value is optionally
// preprocessed with preprocess before expansion (pass nil to skip).
func ExpandMapFunc(values map[string]string, objName string, lookup, preprocess func(string) string) map[string]string {
	vm := newVM()
	_ = vm.Set(objName, vm.NewDynamicObject(&dynamicLookup{
		vm:     vm,
		lookup: lookup,
	}))

	resolved := make(map[string]string, len(values))
	for k, v := range values {
		if preprocess != nil {
			v = preprocess(v)
		}
		resolved[k] = runExpansion(vm, v)
	}
	return resolved
}

// createToolCaller creates a JavaScript function that calls the given tool.
func createToolCaller(ctx context.Context, tool tools.Tool) func(args map[string]any) (string, error) {
	return func(args map[string]any) (string, error) {
		var toolArgs struct {
			Required []string `json:"required"`
		}

		if err := tools.ConvertSchema(tool.Parameters, &toolArgs); err != nil {
			return "", err
		}

		// Filter out nil values for non-required arguments
		nonNilArgs := make(map[string]any)
		for k, v := range args {
			if slices.Contains(toolArgs.Required, k) || v != nil {
				nonNilArgs[k] = v
			}
		}

		arguments, err := json.Marshal(nonNilArgs)
		if err != nil {
			return "", err
		}

		toolCall := tools.ToolCall{
			ID:   "jseval_" + tool.Name,
			Type: "function",
			Function: tools.FunctionCall{
				Name:      tool.Name,
				Arguments: string(arguments),
			},
		}

		if tool.Handler == nil {
			return "", fmt.Errorf("tool '%s' has no handler", tool.Name)
		}

		result, err := tool.Handler(ctx, toolCall)
		if err != nil {
			return "", err
		}

		return result.Output, nil
	}
}

// runExpansion executes the template string using the provided Goja runtime.
// If the full template literal evaluation fails (e.g. because one expression
// references an undefined variable), it falls back to evaluating each ${...}
// expression independently so that successful expressions are still expanded.
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
