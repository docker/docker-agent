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
// At this stage, environment variables are already resolved by Layer 1.
type Expander struct{}

// NewJsExpander creates a new Expander.
func NewJsExpander(_ environment.Provider) *Expander {
	return &Expander{}
}

// Expand evaluates JavaScript template literals.
func (exp *Expander) Expand(_ context.Context, text string, values map[string]string) string {
	if !strings.Contains(text, "${") {
		return text
	}

	vm := newVM()
	for k, v := range values {
		_ = vm.Set(k, v)
	}

	return runExpansion(vm, text)
}

// ExpandMap evaluates JavaScript template literals in all values of the given map.
func (exp *Expander) ExpandMap(_ context.Context, kv map[string]string) map[string]string {
	if kv == nil {
		return nil
	}

	vm := newVM()

	expanded := make(map[string]string, len(kv))
	for k, v := range kv {
		expanded[k] = runExpansion(vm, v)
	}
	return expanded
}

// ExpandCommands evaluates JavaScript template literals in all command fields.
func (exp *Expander) ExpandCommands(_ context.Context, cmds types.Commands) types.Commands {
	if cmds == nil {
		return nil
	}

	vm := newVM()

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
	if err != nil {
		return text
	}

	if v == nil || v.Export() == nil {
		return ""
	}

	return v.String()
}
