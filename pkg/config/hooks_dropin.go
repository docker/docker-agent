package config

import (
	"log/slog"
	"os"
	"path/filepath"

	"github.com/goccy/go-yaml"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/paths"
)

// LoadHookDropIns loads user-level hook drop-in files from the hooks.d
// directory under the config dir. Each *.yaml / *.yml file holds a standalone
// HooksConfig (the same schema as the userconfig `settings.hooks` block).
// Files are merged additively in lexicographic order, so external tools can
// install and uninstall hooks by adding or deleting one self-contained file
// instead of rewriting the shared config.yaml. Returns nil when no drop-in
// hooks exist.
func LoadHookDropIns() *latest.HooksConfig {
	return loadHookDropIns(filepath.Join(paths.GetConfigDir(), "hooks.d"))
}

func loadHookDropIns(dir string) *latest.HooksConfig {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("Failed to read hooks drop-in directory", "dir", dir, "error", err)
		}
		return nil
	}

	// os.ReadDir returns entries sorted by filename, which pins the
	// documented lexicographic merge order.
	var merged *latest.HooksConfig
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if ext := filepath.Ext(entry.Name()); ext != ".yaml" && ext != ".yml" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		hooks, err := readHookDropIn(path)
		if err != nil {
			// A broken drop-in must never break the run: warn and skip.
			slog.Warn("Skipping invalid hooks drop-in file", "path", path, "error", err)
			continue
		}
		merged = MergeHooks(merged, hooks)
	}
	return merged
}

// readHookDropIn parses one drop-in file. Parsing is strict so a typo'd
// event name surfaces as a warning instead of being silently ignored.
func readHookDropIn(path string) (*latest.HooksConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var hooks latest.HooksConfig
	if err := yaml.UnmarshalWithOptions(trimUTF8BOM(data), &hooks, yaml.Strict()); err != nil {
		return nil, err
	}
	if err := hooks.Validate(); err != nil {
		return nil, err
	}
	return &hooks, nil
}
