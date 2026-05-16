package teamloader

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/docker/docker-agent/pkg/cache"
	"github.com/docker/docker-agent/pkg/config/latest"
)

// buildAgentCache turns a per-agent CacheConfig into a [cache.Cache]
// instance, resolving any relative path against parentDir and rejecting
// paths that try to escape it.
//
// The caller is expected to gate this on cfg.Enabled; passing a disabled
// config still returns (nil, nil) so the caller can stay symmetric.
func buildAgentCache(agentName string, cfg *latest.CacheConfig, parentDir string) (*cache.Cache, error) {
	if cfg == nil || !cfg.Enabled {
		return nil, nil
	}

	path, err := resolveCachePath(cfg.Path, parentDir)
	if err != nil {
		return nil, fmt.Errorf("agent %q: %w", agentName, err)
	}

	c, err := cache.New(cache.Config{
		Enabled:       true,
		CaseSensitive: cfg.CaseSensitive,
		TrimSpaces:    cfg.TrimSpaces,
		Path:          path,
	})
	if err != nil {
		return nil, fmt.Errorf("agent %q: initializing response cache: %w", agentName, err)
	}
	return c, nil
}

// resolveCachePath returns path unchanged when it is empty (in-memory
// cache) or absolute; otherwise it joins it with parentDir, cleans the
// result, and refuses any path that escapes parentDir via "..".
//
// Note: This is a lexical check only and does not resolve symlinks. An
// attacker with write access to parentDir could create a symlink that
// points outside the directory tree. This is acceptable because:
//  1. The cache path comes from the agent config, not user input
//  2. An attacker with write access to the config directory already has
//     significant privileges
//  3. The cache only stores agent responses, not sensitive data
func resolveCachePath(path, parentDir string) (string, error) {
	if path == "" || filepath.IsAbs(path) {
		return path, nil
	}
	resolved := filepath.Clean(filepath.Join(parentDir, path))
	cleanParent := filepath.Clean(parentDir) + string(filepath.Separator)
	if !strings.HasPrefix(resolved+string(filepath.Separator), cleanParent) {
		return "", fmt.Errorf("cache path %q escapes parent directory", path)
	}
	return resolved, nil
}
