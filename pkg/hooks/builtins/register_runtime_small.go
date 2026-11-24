//go:build js && wasm && docker_agent_wasm_small

package builtins

import "github.com/docker/docker-agent/pkg/hooks"

// RegisterRuntime installs the stock builtins the small browser runtime uses.
func RegisterRuntime(r *hooks.Registry) error {
	return registerBuiltins(r, portableHooks[:])
}

// RuntimeBuiltinNames returns the names installed by RegisterRuntime.
func RuntimeBuiltinNames() []string {
	names := make([]string, 0, len(portableHooks))
	for _, b := range portableHooks {
		names = append(names, b.name)
	}
	return names
}
