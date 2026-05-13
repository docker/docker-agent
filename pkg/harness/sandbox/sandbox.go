// Package sandbox provides path and environment sandboxing for harness adapters.
// ACP adapters that execute fs/* and terminal/* operations on behalf of the
// harness must confine all file access to the session's working directory.
package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrEscape is returned when a path escapes the sandbox root.
var ErrEscape = errors.New("path escapes sandbox root")

// Resolve resolves path relative to root, rejecting any path that would
// escape root via "..", symlinks, or absolute paths outside root.
//
// Returns the cleaned absolute path on success, or ErrEscape if the
// resolved path is outside root.
func Resolve(root, path string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("sandbox root must not be empty")
	}

	// Resolve root to an absolute, symlink-free path.
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve root: %w", err)
	}
	if resolved, err2 := filepath.EvalSymlinks(absRoot); err2 == nil {
		absRoot = resolved
	}

	// If path is absolute, check it directly.
	var candidate string
	if filepath.IsAbs(path) {
		candidate = filepath.Clean(path)
	} else {
		candidate = filepath.Clean(filepath.Join(absRoot, path))
	}

	// Resolve symlinks in the candidate to prevent symlink escape.
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		if os.IsNotExist(err) {
			// File doesn't exist yet (e.g. a write target). Check the parent.
			parent := filepath.Dir(candidate)
			resolvedParent, err2 := filepath.EvalSymlinks(parent)
			if err2 != nil {
				// Parent doesn't exist either -- check the raw path.
				if !strings.HasPrefix(candidate, absRoot+string(filepath.Separator)) && candidate != absRoot {
					return "", fmt.Errorf("%w: %q is outside %q", ErrEscape, path, root)
				}
				return candidate, nil
			}
			if !strings.HasPrefix(resolvedParent, absRoot+string(filepath.Separator)) && resolvedParent != absRoot {
				return "", fmt.Errorf("%w: %q is outside %q", ErrEscape, path, root)
			}
			return candidate, nil
		}
		return "", fmt.Errorf("eval symlinks: %w", err)
	}

	// Ensure the resolved path is within root.
	if !strings.HasPrefix(resolved, absRoot+string(filepath.Separator)) && resolved != absRoot {
		return "", fmt.Errorf("%w: %q resolves to %q which is outside %q", ErrEscape, path, resolved, root)
	}

	return resolved, nil
}

// AllowedEnv returns a filtered copy of env that removes sensitive variables
// unless they are explicitly listed in allow.
func AllowedEnv(env map[string]string, allow []string) map[string]string {
	allowSet := make(map[string]bool, len(allow))
	for _, k := range allow {
		allowSet[k] = true
	}

	sensitive := map[string]bool{
		"AWS_SECRET_ACCESS_KEY":     true,
		"AWS_SESSION_TOKEN":         true,
		"GOOGLE_APPLICATION_CREDENTIALS": true,
		"AZURE_CLIENT_SECRET":       true,
		"DATABASE_URL":              true,
		"DB_PASSWORD":               true,
		"POSTGRES_PASSWORD":         true,
		"MYSQL_PASSWORD":            true,
		"REDIS_PASSWORD":            true,
		"SECRET_KEY":                true,
		"PRIVATE_KEY":               true,
		"SSH_PRIVATE_KEY":           true,
	}

	out := make(map[string]string, len(env))
	for k, v := range env {
		if sensitive[k] && !allowSet[k] {
			continue
		}
		out[k] = v
	}
	return out
}
