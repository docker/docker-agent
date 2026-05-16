package sandbox

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/docker/docker-agent/pkg/userconfig"
)

// ResolveAlias returns the alias path if name is a user-defined alias,
// or an empty string otherwise.
func ResolveAlias(name string) string {
	cfg, err := userconfig.Load()
	if err != nil {
		return ""
	}
	alias, ok := cfg.GetAlias(name)
	if !ok {
		return ""
	}
	return alias.Path
}

// ExtraWorkspace returns the directory to mount as a read-only extra workspace
// when the agent file lives outside the main workspace. Returns "" if no
// extra mount is needed (file is under wd, is not a local path, etc.).
func ExtraWorkspace(wd, agentRef string) string {
	if agentRef == "" {
		return ""
	}

	// Make the agent reference absolute so we can compare with wd.
	abs, err := filepath.Abs(agentRef)
	if err != nil {
		return ""
	}

	// Only consider paths that look like local files.
	if !looksLikeLocalFile(abs) {
		return ""
	}

	agentDir := filepath.Dir(abs)

	// No extra mount needed if the file is already under the workspace.
	if strings.HasPrefix(agentDir, wd) {
		return ""
	}

	return agentDir
}

// looksLikeLocalFile reports whether path looks like a local agent file
// (has a known config extension or exists on disk).
func looksLikeLocalFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".yaml" || ext == ".yml" || ext == ".hcl" {
		return true
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
