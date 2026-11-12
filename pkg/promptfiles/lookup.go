// Package promptfiles centralises the search rules used to resolve
// add_prompt_files entries (typically AGENTS.md, CLAUDE.md, ...). The
// rules are shared by:
//
//   - the runtime add_prompt_files hook, which executes inside the
//     sandbox at turn-start;
//   - the docker-agent kit builder, which walks the same paths on the
//     host before sandbox launch so it can stage every relevant file
//     into the kit (see pkg/sandbox/kit).
//
// Keeping the rules in one place guarantees both ends pick the same
// files and avoids drift when, for example, the kit ships a redacted
// copy of ~/AGENTS.md that the in-sandbox lookup must then prefer over
// the now-missing host $HOME entry.
package promptfiles

import (
	"errors"
	"os"
	"path/filepath"
	"slices"

	"github.com/docker/docker-agent/pkg/skills"
)

// InstructionGroup identifies prompt-file sources in persisted instruction context.
const InstructionGroup = "core/prompt-files"

// KitSubdir is the subdirectory inside a docker-agent kit that holds
// staged prompt files. The host writes to it; the in-sandbox lookup
// reads from it.
const KitSubdir = "prompt_files"

// Paths returns the prompt-file paths to load for filename, in order:
//
//  1. The closest match found while walking up from workDir, if any.
//  2. Either a kitDir match (when kitDir is non-empty — running inside
//     a sandbox with a staged kit), or a homeDir match.
//
// kitDir takes precedence over homeDir because, in the sandbox, $HOME
// does not contain the host's prompt files; the kit is what brought
// them along (already redacted). Returns at most two paths. Passing
// homeDir == "" disables the home-dir lookup — useful in tests so
// they don't need to touch the real $HOME.
func Paths(workDir, homeDir, kitDir, filename string) []string {
	paths, _ := PathsWithError(workDir, homeDir, kitDir, filename)
	return paths
}

// PathsWithError preserves partial matches but reports non-missing discovery failures.
func PathsWithError(workDir, homeDir, kitDir, filename string) ([]string, error) {
	var paths []string
	p, lookupErr := findInHierarchy(workDir, filename)
	if p != "" {
		paths = append(paths, p)
	}
	var extra string
	switch {
	case kitDir != "":
		extra = filepath.Join(kitDir, KitSubdir, filename)
	case homeDir != "":
		extra = filepath.Join(homeDir, filename)
	}
	if extra != "" {
		exists, err := fileExists(extra)
		lookupErr = errors.Join(lookupErr, err)
		if exists && !slices.Contains(paths, extra) {
			paths = append(paths, extra)
		}
	}
	return paths, lookupErr
}

// PathsFromEnv is a convenience wrapper around Paths that reads the
// kit directory from [skills.KitDirEnv]. The runtime in-sandbox hook
// uses it; host-side callers (the kit builder) pass kitDir explicitly
// to Paths instead.
func PathsFromEnv(workDir, homeDir, filename string) []string {
	return Paths(workDir, homeDir, os.Getenv(skills.KitDirEnv), filename)
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
