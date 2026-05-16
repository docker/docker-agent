package fsx

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// CollectFiles recursively collects all files from given paths.
// Supports glob patterns (via doublestar), directories, and individual files.
// Skips paths that don't exist instead of returning an error.
// Optional shouldIgnore filter can exclude files/directories (return true to skip).
// Respects context cancellation.
func CollectFiles(ctx context.Context, paths []string, shouldIgnore func(path string) bool) ([]string, error) {
	var files []string
	seen := make(map[string]bool)

	for _, pattern := range paths {
		// Check for context cancellation
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		expanded, err := expandPattern(pattern)
		if err != nil {
			return nil, err
		}
		if len(expanded) == 0 {
			expanded = []string{pattern}
		}

		for _, entry := range expanded {
			// Check for context cancellation
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
			}
			normalized := normalizePath(entry)

			// Check if this path should be ignored
			if shouldIgnore != nil && shouldIgnore(normalized) {
				continue
			}

			info, err := os.Stat(normalized)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return nil, fmt.Errorf("failed to stat %s: %w", entry, err)
			}

			if info.IsDir() {
				// Use DirectoryTree to collect files from directory
				tree, err := DirectoryTree(ctx, normalized, func(string) error { return nil }, shouldIgnore, 0)
				if err != nil {
					return nil, fmt.Errorf("failed to read directory %s: %w", normalized, err)
				}
				// Traverse tree and collect absolute file paths
				var dirFiles []string
				CollectFilesFromTree(tree, filepath.Dir(normalized), &dirFiles)
				for _, f := range dirFiles {
					// Check for context cancellation
					select {
					case <-ctx.Done():
						return nil, ctx.Err()
					default:
					}

					absPath := normalizePath(f)
					if !seen[absPath] {
						files = append(files, absPath)
						seen[absPath] = true
					}
				}
				continue
			}

			if !seen[normalized] {
				files = append(files, normalized)
				seen[normalized] = true
			}
		}
	}

	return files, nil
}

// Matches reports whether the given path matches any configured path or glob pattern.
// Useful for file watchers to determine if a changed file matches configured patterns.
func Matches(path string, patterns []string) (bool, error) {
	if len(patterns) == 0 {
		return false, nil
	}

	cleanPath := normalizePath(path)

	for _, pattern := range patterns {
		if pattern == "" {
			continue
		}

		normalizedPattern := normalizePath(pattern)

		if hasGlob(pattern) {
			match, err := doublestar.PathMatch(normalizedPattern, cleanPath)
			if err != nil {
				return false, fmt.Errorf("invalid glob pattern %q: %w", pattern, err)
			}
			if match {
				return true, nil
			}
			continue
		}

		info, err := os.Stat(normalizedPattern)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return false, fmt.Errorf("failed to stat %s: %w", normalizedPattern, err)
		}

		if info.IsDir() {
			if cleanPath == normalizedPattern || strings.HasPrefix(cleanPath, normalizedPattern+string(os.PathSeparator)) {
				return true, nil
			}
			continue
		}

		if cleanPath == normalizedPattern {
			return true, nil
		}
	}

	return false, nil
}

func expandPattern(pattern string) ([]string, error) {
	if !hasGlob(pattern) {
		return []string{normalizePath(pattern)}, nil
	}

	matches, err := doublestar.FilepathGlob(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid glob pattern %q: %w", pattern, err)
	}

	results := make([]string, 0, len(matches))
	for _, match := range matches {
		results = append(results, normalizePath(match))
	}

	return results, nil
}

func hasGlob(pattern string) bool {
	return strings.ContainsAny(pattern, "*?[")
}

func normalizePath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(p)
}
