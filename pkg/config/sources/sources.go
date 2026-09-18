// Package sources resolves agent references — local files and directories,
// built-in agents, user-config aliases, URLs and OCI references — to
// config.Source values. It links every source type, so embedders that only
// need one should construct it directly (config.NewFileSource,
// ocisource.New, ...) instead of importing this package.
package sources

import (
	"cmp"
	_ "embed"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/hcl"
	"github.com/docker/docker-agent/pkg/config/ocisource"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/reference"
	"github.com/docker/docker-agent/pkg/userconfig"
)

//go:embed builtin-agents/default.yaml
var defaultAgent []byte

//go:embed builtin-agents/coder.yaml
var coderAgent []byte

// builtinAgents maps built-in agent names to their embedded YAML configurations.
var builtinAgents = map[string][]byte{
	"default": defaultAgent,
	"coder":   coderAgent,
}

// BuiltinAgentNames returns the names of all built-in agents.
func BuiltinAgentNames() []string {
	return slices.Sorted(maps.Keys(builtinAgents))
}

// ResolveAlias resolves an agent reference and returns the alias if it exists and has options.
// Returns nil if the reference is not an alias or doesn't have options.
func ResolveAlias(agentFilename string) *userconfig.Alias {
	agentFilename = cmp.Or(agentFilename, "default")

	cfg, err := userconfig.Load()
	if err != nil {
		slog.Warn("Failed to load user config; aliases are unavailable", "error", err)
		return nil
	}

	alias, ok := cfg.GetAlias(agentFilename)
	if !ok || !alias.HasOptions() {
		return nil
	}

	return alias
}

// ResolveSources resolves an agent file reference (local file or directory, URL, or OCI image) to sources.
// If envProvider is non-nil, it will be used to look up GITHUB_TOKEN for authentication
// when fetching from GitHub URLs.
// For OCI references, always checks remote for updates but falls back to local cache if offline;
// ociOpts (e.g. ocisource.WithVerificationKey) apply to them and are ignored for other kinds.
func ResolveSources(agentsPath string, envProvider environment.Provider, ociOpts ...ocisource.Option) (config.Sources, error) {
	resolvedPath, err := resolve(agentsPath)
	if err != nil {
		// resolve() only fails for non-OCI, non-URL, non-builtin references
		// that can't be made absolute. Try OCI as last resort.
		if config.IsOCIReference(agentsPath) {
			return singleSource(reference.OciRefToFilename(agentsPath), ociSource(agentsPath, ociOpts)), nil
		}
		return nil, err
	}

	// Only directories need special handling to enumerate YAML files.
	if dirExists(resolvedPath) {
		return resolveDirectory(resolvedPath, envProvider)
	}

	// For all other reference types, delegate to resolveOne.
	key, source := resolveOne(resolvedPath, envProvider, ociOpts)
	return singleSource(key, source), nil
}

// Resolve resolves an agent file reference (local file, URL, or OCI image) to a source.
// If envProvider is non-nil, it will be used to look up GITHUB_TOKEN for authentication
// when fetching from GitHub URLs.
// For OCI references, always checks remote for updates but falls back to local cache if offline;
// ociOpts (e.g. ocisource.WithVerificationKey) apply to them and are ignored for other kinds.
func Resolve(agentFilename string, envProvider environment.Provider, ociOpts ...ocisource.Option) (config.Source, error) {
	resolvedPath, err := resolve(agentFilename)
	if err != nil {
		if config.IsOCIReference(agentFilename) {
			return ociSource(agentFilename, ociOpts), nil
		}
		return nil, err
	}

	_, source := resolveOne(resolvedPath, envProvider, ociOpts)
	return source, nil
}

// resolveOne maps a resolved path to the appropriate Source and a key for use
// in Sources maps. The path must already be resolved via resolve().
// This is the single place that decides which source type a reference maps to.
// To add a new source type, add a case here.
func resolveOne(resolvedPath string, envProvider environment.Provider, ociOpts []ocisource.Option) (string, config.Source) {
	switch {
	case builtinAgents[resolvedPath] != nil:
		return resolvedPath, config.NewBytesSource(resolvedPath, builtinAgents[resolvedPath])
	case config.IsURLReference(resolvedPath):
		// URL-encode the URL to make it safe for use as a map key
		return url.QueryEscape(resolvedPath), hcl.NewSource(config.NewURLSource(resolvedPath, envProvider))
	case isLocalFile(resolvedPath):
		return fileNameWithoutExt(resolvedPath), hcl.NewSource(config.NewFileSource(resolvedPath))
	default:
		return reference.OciRefToFilename(resolvedPath), ociSource(resolvedPath, ociOpts)
	}
}

// ociSource builds an OCI source; artifacts may carry HCL as well as YAML.
func ociSource(ref string, opts []ocisource.Option) config.Source {
	return hcl.NewSource(ocisource.New(ref, opts...))
}

// resolveDirectory enumerates YAML files in a directory and resolves each one.
func resolveDirectory(dirPath string, envProvider environment.Provider) (config.Sources, error) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return nil, fmt.Errorf("reading agents directory %s: %w", dirPath, err)
	}

	sources := make(config.Sources)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		if ext != ".yaml" && ext != ".yml" && ext != ".hcl" {
			continue
		}
		a := filepath.Join(dirPath, entry.Name())
		sources[fileNameWithoutExt(a)], err = Resolve(a, envProvider)
		if err != nil {
			return nil, err
		}
	}
	return sources, nil
}

// singleSource wraps a single source in a Sources map.
func singleSource(key string, source config.Source) config.Sources {
	return config.Sources{key: source}
}

// resolve resolves an agent reference, handling aliases and defaults
func resolve(agentFilename string) (string, error) {
	agentFilename = cmp.Or(agentFilename, "default")

	// Try to resolve as an alias first
	if cfg, err := userconfig.Load(); err == nil {
		if alias, ok := cfg.GetAlias(agentFilename); ok {
			slog.Debug("Resolved alias", "alias", agentFilename, "path", alias.Path)
			agentFilename = alias.Path
		}
	} else {
		slog.Warn("Failed to load user config; aliases are unavailable", "error", err)
	}

	// Built-in agent names (e.g. "default", "coder") are either user defined aliases or embedded agents
	if _, ok := builtinAgents[agentFilename]; ok {
		return agentFilename, nil
	}

	// Don't convert OCI references or URLs to absolute paths
	if config.IsOCIReference(agentFilename) || config.IsURLReference(agentFilename) {
		return agentFilename, nil
	}

	abs, err := filepath.Abs(agentFilename)
	if err != nil {
		return "", err
	}

	return abs, nil
}

// isLocalFile checks if the input is a local file
func isLocalFile(input string) bool {
	ext := strings.ToLower(filepath.Ext(input))
	// Check for known config file extensions or file descriptors
	if ext == ".yaml" || ext == ".yml" || ext == ".hcl" || strings.HasPrefix(input, "/dev/fd/") {
		return true
	}
	// Check if it exists as a file on disk
	return fileExists(input)
}

// fileExists checks if a file exists at the given path
func fileExists(path string) bool {
	s, err := os.Stat(path)
	return err == nil && !s.IsDir()
}

// dirExists checks if a directory exists at the given path
func dirExists(path string) bool {
	s, err := os.Stat(path)
	return err == nil && s.IsDir()
}

func fileNameWithoutExt(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}
