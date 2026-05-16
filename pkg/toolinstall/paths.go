package toolinstall

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/docker/docker-agent/pkg/paths"
)

// ToolsDir returns the base directory for installed tools.
// Checks DOCKER_AGENT_TOOLS_DIR env var, defaults to ~/.cagent/tools/
func ToolsDir() string {
	if dir := os.Getenv("DOCKER_AGENT_TOOLS_DIR"); dir != "" {
		return filepath.Clean(dir)
	}
	return filepath.Join(paths.GetDataDir(), "tools")
}

// BinDir returns the directory where tool binaries/symlinks are placed.
// This is ToolsDir()/bin/
func BinDir() string {
	return filepath.Join(ToolsDir(), "bin")
}

// PackageDir returns the directory for a specific package version.
// This is ToolsDir()/packages/<owner>/<repo>/<version>/
func PackageDir(owner, repo, version string) string {
	return filepath.Join(ToolsDir(), "packages", owner, repo, version)
}

// RegistryDir returns the directory for cached registry data.
func RegistryDir() string {
	return filepath.Join(ToolsDir(), "registry")
}

// PrependBinDirToEnv takes an env slice and ensures the tools bin directory
// is prepended to the PATH entry. This allows installed tools to find
// other installed tools (e.g., npx finding node).
func PrependBinDirToEnv(env []string) []string {
	binDir := BinDir()
	result := make([]string, 0, len(env))
	found := false

	for _, e := range env {
		if existing, ok := strings.CutPrefix(e, "PATH="); ok {
			result = append(result, "PATH="+binDir+string(os.PathListSeparator)+existing)
			found = true
		} else {
			result = append(result, e)
		}
	}

	if !found {
		result = append(result, "PATH="+binDir)
	}

	return result
}
