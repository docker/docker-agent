package mcp

import "context"

// No process can run under js/wasm, so there is nothing to install. Leaving
// the installer out also keeps its expr and text/template engines, whose
// reflective method lookups defeat the linker's method pruning, out of
// browser builds.
func ensureCommand(_ context.Context, command, _ string) (string, error) {
	return command, nil
}

func prependBinDirToEnv(env []string) []string {
	return env
}
