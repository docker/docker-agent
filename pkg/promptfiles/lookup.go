// Package promptfiles resolves add_prompt_files entries such as AGENTS.md.
package promptfiles

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
)

// InstructionGroup identifies prompt-file sources in persisted instruction context.
const InstructionGroup = "core/prompt-files"

// Paths returns the prompt-file paths to load for filename, in order:
//
//  1. The closest match found while walking up from workDir, if any.
//  2. A match in homeDir, unless already found above.
func Paths(workDir, homeDir, filename string) []string {
	paths, _ := PathsWithError(workDir, homeDir, filename)
	return paths
}

// PathsWithError preserves partial matches but reports non-missing discovery failures.
func PathsWithError(workDir, homeDir, filename string) ([]string, error) {
	var paths []string
	p, lookupErr := findInHierarchy(workDir, filename)
	if p != "" {
		paths = append(paths, p)
	}
	if homeDir != "" {
		extra := filepath.Join(homeDir, filename)
		exists, err := fileExists(extra)
		lookupErr = errors.Join(lookupErr, err)
		if exists && !slices.Contains(paths, extra) {
			paths = append(paths, extra)
		}
	}
	return paths, lookupErr
}

func findInHierarchy(startDir, filename string) (string, error) {
	dir, err := filepath.Abs(startDir)
	if err != nil {
		return "", err
	}
	var lookupErr error
	for {
		path := filepath.Join(dir, filename)
		exists, err := fileExists(path)
		lookupErr = errors.Join(lookupErr, err)
		if exists {
			return path, lookupErr
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", lookupErr
		}
		dir = parent
	}
}

// isFile reports whether path exists and is a regular file.
func isFile(path string) bool {
	exists, _ := fileExists(path)
	return exists
}

func fileExists(path string) (bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return !info.IsDir(), nil
}
