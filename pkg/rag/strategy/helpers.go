package strategy

import (
	"cmp"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/fsx"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/rag/types"
)

// Helper functions for common operations

// MergeDocPaths merges shared docs with strategy-specific docs and makes them absolute
func MergeDocPaths(sharedDocs, strategyDocs []string, parentDir string) []string {
	seen := make(map[string]bool)
	var result []string

	// Add shared docs first
	for _, doc := range sharedDocs {
		absPath := makeAbsolute(doc, parentDir)
		if !seen[absPath] {
			seen[absPath] = true
			result = append(result, absPath)
		}
	}

	// Add strategy-specific docs
	for _, doc := range strategyDocs {
		absPath := makeAbsolute(doc, parentDir)
		if !seen[absPath] {
			seen[absPath] = true
			result = append(result, absPath)
		}
	}

	return result
}

// ResolveDatabasePath resolves database configuration to a path
func ResolveDatabasePath(dbCfg latest.RAGDatabaseConfig, parentDir, defaultName string) (string, error) {
	if dbCfg.IsEmpty() {
		// Use default path in data directory
		return filepath.Join(paths.GetDataDir(), defaultName), nil
	}

	dbStr, err := dbCfg.AsString()
	if err != nil {
		return "", err
	}

	// If it's a connection string (has ://), return as-is
	if strings.Contains(dbStr, "://") {
		return dbStr, nil
	}

	// If it's a relative file path, make it absolute
	if !filepath.IsAbs(dbStr) {
		if parentDir == "" {
			slog.Debug("Resolving relative database path with empty parentDir, using working directory", "path", dbStr)
			abs, err := filepath.Abs(dbStr)
			if err != nil {
				return "", fmt.Errorf("failed to resolve absolute path for %q: %w", dbStr, err)
			}
			return abs, nil
		}
		return filepath.Join(parentDir, dbStr), nil
	}

	return dbStr, nil
}

// GetParam gets a parameter from the config Params map.
// It includes numeric coercion so YAML numbers (which may be decoded as int, int64,
// uint64 or float64) can be safely read as either int or float64 without callers
// needing to worry about the concrete type.
func GetParam[T any](params map[string]any, key string, defaultValue T) T {
	raw, ok := params[key]
	if !ok {
		return defaultValue
	}

	var zero T

	switch any(zero).(type) {
	case int:
		switch v := raw.(type) {
		case int:
			return any(v).(T)
		case int64:
			return any(int(v)).(T)
		case uint64:
			return any(int(v)).(T) //nolint:gosec // value comes from validated config; bounds enforced upstream
		case float64:
			return any(int(v)).(T)
		default:
			return defaultValue
		}
	case float64:
		switch v := raw.(type) {
		case float64:
			return any(v).(T)
		case int:
			return any(float64(v)).(T)
		case int64:
			return any(float64(v)).(T)
		case uint64:
			return any(float64(v)).(T)
		default:
			return defaultValue
		}
	default:
		if typed, ok := raw.(T); ok {
			return typed
		}
		return defaultValue
	}
}

// GetParamPtr gets a parameter pointer from the config Params map
func GetParamPtr[T any](params map[string]any, key string) *T {
	raw, ok := params[key]
	if !ok {
		return nil
	}

	var zero T

	switch any(zero).(type) {
	case int:
		switch v := raw.(type) {
		case int:
			val := any(v).(T)
			return &val
		case int64:
			val := any(int(v)).(T)
			return &val
		case uint64:
			val := any(int(v)).(T) //nolint:gosec // value comes from validated config; bounds enforced upstream
			return &val
		case float64:
			val := any(int(v)).(T)
			return &val
		default:
			return nil
		}
	case float64:
		switch v := raw.(type) {
		case float64:
			val := any(v).(T)
			return &val
		case int:
			val := any(float64(v)).(T)
			return &val
		case int64:
			val := any(float64(v)).(T)
			return &val
		case uint64:
			val := any(float64(v)).(T)
			return &val
		default:
			return nil
		}
	default:
		if typed, ok := raw.(T); ok {
			return &typed
		}
		return nil
	}
}

// makeAbsolute makes a path absolute relative to parentDir.
// If parentDir is empty (e.g. for OCI/URL sources), the path is resolved
// against the current working directory.
func makeAbsolute(p, parentDir string) string {
	if filepath.IsAbs(p) {
		return p
	}
	if parentDir == "" {
		slog.Debug("Resolving relative path with empty parentDir, using working directory", "path", p)
		abs, err := filepath.Abs(p)
		if err != nil {
			slog.Warn("Failed to resolve absolute path, using as-is", "path", p, "error", err)
			return p
		}
		return abs
	}
	return filepath.Join(parentDir, p)
}

// EmitEvent sends an event to the events channel using non-blocking send
// This prevents strategies from hanging if the event channel is full or not ready
// Automatically sets the StrategyName field in the event
func EmitEvent(events chan<- types.Event, event types.Event, strategyName string) {
	if events != nil {
		// Set the strategy name in the event
		event.StrategyName = strategyName

		select {
		case events <- event:
		default:
			// Channel full or not ready, drop event to avoid blocking
			slog.Warn("RAG event channel full, dropping event", "strategy", strategyName, "event_type", event.Type)
		}
	}
}

// ResolveModelConfig resolves a model reference into a ModelConfig.
// Supports "provider/model" inline references or named references into the models map.
func ResolveModelConfig(ref string, models map[string]latest.ModelConfig) (latest.ModelConfig, error) {
	// Try inline "provider/model" format first
	if parts := strings.SplitN(ref, "/", 2); len(parts) == 2 {
		return latest.ModelConfig{
			Provider: parts[0],
			Model:    parts[1],
		}, nil
	}

	// Try named reference
	if cfg, ok := models[ref]; ok {
		return cfg, nil
	}

	return latest.ModelConfig{}, fmt.Errorf("model %q not found", ref)
}

// ParseChunkingConfig extracts chunking configuration from RAGStrategyConfig.
func ParseChunkingConfig(cfg latest.RAGStrategyConfig) ChunkingConfig {
	chunkSize := cfg.Chunking.Size
	if chunkSize == 0 {
		// Use larger default for code-aware chunking since TreeSitter groups
		// complete functions and benefits from more context
		if cfg.Chunking.CodeAware {
			chunkSize = 4000 // Code-aware: captures 2-4 medium functions per chunk
		} else {
			chunkSize = 1500 // General text: good paragraph/section size
		}
	}

	chunkOverlap := cmp.Or(cfg.Chunking.Overlap, 75)

	return ChunkingConfig{
		Size:                  chunkSize,
		Overlap:               chunkOverlap,
		RespectWordBoundaries: cfg.Chunking.RespectWordBoundaries,
		CodeAware:             cfg.Chunking.CodeAware,
	}
}

// BuildShouldIgnore creates a filter function based on BuildContext and optional strategy-level override.
// Strategy params can override the RAG-level respect_vcs setting.
// Returns nil if no filtering should be applied.
func BuildShouldIgnore(buildCtx BuildContext, strategyParams map[string]any) func(path string) bool {
	// Check for strategy-level override first
	respectVCS := buildCtx.RespectVCS
	if strategyParams != nil {
		if override, ok := strategyParams["respect_vcs"].(bool); ok {
			respectVCS = override
		}
	}

	if !respectVCS {
		return nil
	}

	// Try to create a VCS matcher for ignore file support (e.g., .gitignore)
	matcher, err := fsx.NewVCSMatcher(buildCtx.ParentDir)
	if err != nil {
		slog.Warn("Failed to initialize VCS matcher", "error", err)
		return nil
	}
	if matcher == nil {
		// No VCS repository found - this is normal, not an error
		return nil
	}

	slog.Debug("VCS ignore filtering enabled", "repo_root", matcher.RepoRoot())
	return matcher.ShouldIgnore
}
