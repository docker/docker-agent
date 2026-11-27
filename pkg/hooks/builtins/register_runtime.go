//go:build !js || !wasm || !docker_agent_wasm_small

package builtins

import "github.com/docker/docker-agent/pkg/hooks"

// RegisterRuntime installs the stock builtin hooks for the runtime's build profile.
func RegisterRuntime(r *hooks.Registry) error {
	return Register(r)
}
