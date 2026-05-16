//go:build js && wasm

package desktop

// getDockerDesktopPaths returns empty paths under js/wasm: the Docker Desktop
// backend speaks over Unix domain sockets / named pipes, neither of which is
// reachable from a browser. The runtime falls back to no Desktop integration.
func getDockerDesktopPaths() (DockerDesktopPaths, error) {
	return DockerDesktopPaths{}, nil
}

// IsWSL is always false under js/wasm.
func IsWSL() bool {
	return false
}
