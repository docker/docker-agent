//go:build !js

package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/memory/database/sqlite"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/toolsetpath"
)

// CreateToolSet is used by the tools registry.
func CreateToolSet(toolset latest.Toolset, parentDir string, runConfig *config.RuntimeConfig, configName string) (tools.ToolSet, error) {
	dbPath, err := databasePath(toolset, parentDir, runConfig, configName)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return nil, fmt.Errorf("failed to create memory database directory: %w", err)
	}

	db, err := sqlite.NewMemoryDatabase(dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create memory database: %w", err)
	}

	return NewWithPath(db, dbPath), nil
}

// databasePath resolves the memory database path from the toolset
// declaration, defaulting to a per-config file under the data directory.
func databasePath(toolset latest.Toolset, parentDir string, runConfig *config.RuntimeConfig, configName string) (string, error) {
	if toolset.Path != "" {
		validatedMemoryPath, err := toolsetpath.Resolve(toolset.Path, parentDir, runConfig)
		if err != nil {
			return "", fmt.Errorf("invalid memory database path: %w", err)
		}
		return validatedMemoryPath, nil
	}

	if configName == "" {
		configName = "default"
	}
	return filepath.Join(paths.GetDataDir(), "memory", sanitizePathSegment(configName), "memory.db"), nil
}

// sanitizePathSegment replaces characters that are illegal in a single path
// component on Windows with '_'. Agent sources loaded from an OCI reference
// (e.g. "namespace/repo:tag") produce config names that include the image
// tag's ':'; the colon causes os.MkdirAll to fail with ERROR_INVALID_NAME on
// NTFS. The replacement is lossy but safe — the hash suffix already in the
// config name preserves uniqueness, so collisions from sanitisation aren't a
// concern in practice.
func sanitizePathSegment(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '<', '>', ':', '"', '|', '?', '*', '\\', '/':
			return '_'
		}
		if r < 0x20 {
			return '_'
		}
		return r
	}, s)
}
