package builtins

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// isGitRepo checks if the given directory or one of its parents is a git
// repository. Used by the add_environment_info builtin to surface git
// awareness to the model.
func isGitRepo(dir string) bool {
	if dir == "" {
		return false
	}

	current, err := filepath.Abs(dir)
	if err != nil {
		return false
	}

	for {
		info, err := os.Stat(filepath.Join(current, ".git"))
		if err != nil {
			if !os.IsNotExist(err) {
				return false
			}
		} else if info.IsDir() {
			return true
		}

		parent := filepath.Dir(current)
		if parent == current {
			return false
		}
		current = parent
	}
}

// maxGitOutputBytes caps the captured output of any git invocation made
// by a builtin. Builtins inject their result into every model call, so
// unbounded output (large diffs, long histories) would silently inflate
// prompt cost.
const maxGitOutputBytes = 4096

// gitOutput runs `git -C dir args...` and returns the trimmed stdout,
// truncated to [maxGitOutputBytes] with a "... (truncated)" suffix.
//
// Returns ("", err) when git isn't on PATH or the command fails (e.g.
// dir isn't a git repo). Callers are expected to guard against an
// empty dir before calling.
func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	if dir == "" {
		// Defensive: every caller guards on Cwd, but bailing out
		// here keeps a future caller from accidentally running git
		// in the process's working directory.
		return "", errors.New("empty working directory")
	}
	full := append([]string{"-C", dir}, args...)
	out, err := exec.CommandContext(ctx, "git", full...).Output()
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(out))
	if len(s) > maxGitOutputBytes {
		s = s[:maxGitOutputBytes] + "\n... (truncated)"
	}
	return s, nil
}
