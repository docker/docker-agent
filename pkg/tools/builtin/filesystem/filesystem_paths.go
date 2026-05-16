package filesystem

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	pathx "github.com/docker/docker-agent/pkg/path"
)

// pathRootSet is a set of filesystem roots, used to back the filesystem
// toolset's allow- and deny-lists. Each entry is expanded once at construction
// time (token "." -> working directory, token "~" or leading "~/" -> user
// home directory, environment variables -> their values), then resolved to an
// absolute, symlink-free real path.
//
// An optional [*os.Root] is opened per entry. When available, it is used to
// confirm path containment via the kernel's rooted-lookup semantics, which
// reject symlink and ".." escapes regardless of the on-disk layout
// (TOCTOU-safe). Falls back to a lexical prefix check when [*os.Root] cannot
// be opened (root does not exist yet, restricted permissions, …).
type pathRootSet struct {
	entries []pathRoot
}

type pathRoot struct {
	// raw is the original, un-expanded entry from the user configuration.
	// Kept for error messages so that violations report what the user wrote.
	raw string
	// real is the absolute path with all symlinks resolved. May equal the
	// expanded path when the entry does not yet exist; in that case root is
	// nil and we fall back to a lexical prefix check.
	real string
	// root is an [*os.Root] handle for real, lazily set when [os.OpenRoot]
	// succeeds. Used to make containment checks TOCTOU-safe.
	root *os.Root
}

// newPathRootSet expands the supplied tokens against workingDir and returns a
// pathRootSet. Returns nil for an empty input — callers should treat a nil
// set as "no constraint applies".
//
// Recognised tokens:
//   - "." resolves to workingDir.
//   - "~" or a leading "~/" resolves against the user's home directory.
//   - "$VAR" / "${VAR}" expands environment variables.
//   - Any other relative path is resolved against workingDir.
//   - Absolute paths are kept as-is.
func newPathRootSet(workingDir string, tokens []string) (*pathRootSet, error) {
	if len(tokens) == 0 {
		return nil, nil
	}
	set := &pathRootSet{}
	seen := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		entry, err := newPathRoot(workingDir, token)
		if err != nil {
			// Release any roots opened so far before returning. The toolset
			// constructor swallows the error and falls back to "no list",
			// so leaving open file descriptors here would leak silently.
			set.close()
			return nil, err
		}
		if _, dup := seen[entry.real]; dup {
			// Duplicate of an earlier entry: release the *os.Root we just
			// opened to avoid leaking the file descriptor.
			if entry.root != nil {
				_ = entry.root.Close()
			}
			continue
		}
		seen[entry.real] = struct{}{}
		set.entries = append(set.entries, entry)
	}
	return set, nil
}

// newPathRoot resolves a single token to a pathRoot. Errors when the token
// is empty (before or after env-var expansion) or its home/working directory
// expansion fails. Non-existent target directories are accepted (root is
// left nil and we fall back to the lexical check) so that the agent can
// still operate when, e.g., "~/projects" hasn't been created yet.
func newPathRoot(workingDir, token string) (pathRoot, error) {
	expanded, err := expandPathToken(workingDir, token)
	if err != nil {
		return pathRoot{}, fmt.Errorf("%s: %w", token, err)
	}

	abs, err := filepath.Abs(expanded)
	if err != nil {
		return pathRoot{}, fmt.Errorf("resolving %q: %w", token, err)
	}

	realPath, err := resolveRealPath(abs)
	if err != nil {
		return pathRoot{}, fmt.Errorf("resolving %q: %w", token, err)
	}

	entry := pathRoot{raw: token, real: realPath}
	if root, err := os.OpenRoot(realPath); err == nil {
		entry.root = root
	} else if !errors.Is(err, fs.ErrNotExist) {
		// Log unexpected failures but don't error out: lexical containment
		// is still enforced below.
		slog.Debug("filesystem allow/deny: os.OpenRoot failed; falling back to lexical check",
			"path", realPath, "error", err)
	}
	return entry, nil
}

// expandPathToken resolves "." / "~" / "$VAR" tokens and joins relative paths
// with workingDir. It does not resolve symlinks or canonicalise the result.
//
// Returns an error when the token is empty before OR after environment
// variable expansion. The post-expansion check matters because
// [os.ExpandEnv] silently substitutes undefined variables with the empty
// string — without rejection, an unset "$NOPE" entry would resolve to the
// working directory and silently widen an allow-list (or close a deny-list)
// in surprising ways.
func expandPathToken(workingDir, token string) (string, error) {
	// Trim spaces but keep internal whitespace untouched (some macOS paths
	// contain spaces, e.g. "~/Library/Application Support").
	original := strings.TrimSpace(token)
	if original == "" {
		return "", errors.New("path entry must not be empty")
	}
	token = os.ExpandEnv(original)
	if strings.TrimSpace(token) == "" {
		return "", fmt.Errorf("path entry %q expands to an empty string (undefined environment variable?)", original)
	}

	if expandedToken, err := pathx.ExpandHomeDir(token); err != nil {
		return "", err
	} else if expandedToken != token || token == "~" {
		return expandedToken, nil
	}

	switch {
	case token == ".":
		if workingDir == "" {
			return os.Getwd()
		}
		return workingDir, nil
	case filepath.IsAbs(token):
		return token, nil
	default:
		if workingDir == "" {
			return token, nil
		}
		return filepath.Join(workingDir, token), nil
	}
}

// entryFor returns the entry that lexically contains realAbsPath plus the
// slash-separated relative path within it. realAbsPath must already be a
// canonical (symlink-resolved) absolute path — see [resolveRealPath].
// Returns (nil, "") when the path is outside every entry.
func (rs *pathRootSet) entryFor(realAbsPath string) (*pathRoot, string) {
	if rs == nil {
		return nil, ""
	}
	for i := range rs.entries {
		entry := &rs.entries[i]
		rel, err := filepath.Rel(entry.real, realAbsPath)
		if err != nil {
			continue
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue // lexically outside this root
		}
		return entry, filepath.ToSlash(rel)
	}
	return nil, ""
}

// contains reports whether absPath is inside any of the roots in the set.
// absPath must be absolute and symlink-resolved (see [resolveRealPath]).
//
// When the matching entry has an [*os.Root] handle we additionally probe
// the path through it: the kernel will reject any access that escapes the
// root via an absolute symlink or a relative symlink that climbs above the
// boundary, regardless of timing or on-disk changes. Non-existent paths
// are accepted (the caller may be about to create them) — a subsequent
// write goes through [resolveAndCheckPath] again before any I/O.
func (rs *pathRootSet) contains(absPath string) bool {
	entry, rel := rs.entryFor(absPath)
	if entry == nil {
		return false
	}
	if entry.root == nil || rel == "." {
		return true
	}
	_, err := entry.root.Lstat(rel)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return true
	}
	// e.g. ELOOP from a symlink that escapes the root: treat as outside.
	slog.Debug("filesystem allow/deny: rooted Lstat rejected path",
		"root", entry.real, "rel", rel, "error", err)
	return false
}

// describe returns a comma-separated, human-readable list of the entries.
// Used in error messages and tool instructions.
func (rs *pathRootSet) describe() string {
	if rs == nil || len(rs.entries) == 0 {
		return ""
	}
	parts := make([]string, len(rs.entries))
	for i, e := range rs.entries {
		parts[i] = e.raw
	}
	return strings.Join(parts, ", ")
}

// close releases any [*os.Root] handles owned by the set. Safe to call on a
// nil receiver.
func (rs *pathRootSet) close() {
	if rs == nil {
		return
	}
	for i := range rs.entries {
		if rs.entries[i].root != nil {
			_ = rs.entries[i].root.Close()
			rs.entries[i].root = nil
		}
	}
}

// resolveRealPath returns the absolute, symlink-resolved form of p. If p does
// not (yet) exist, it walks up to the nearest existing ancestor, resolves
// symlinks on that ancestor, and re-appends the missing tail. This lets the
// allow/deny check work for paths that are about to be created (write_file,
// create_directory, …) without falsely accepting a path that would, once
// created, escape the boundary.
func resolveRealPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if realPath, err := filepath.EvalSymlinks(abs); err == nil {
		return realPath, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	// Walk up to find the nearest existing ancestor.
	parent := filepath.Dir(abs)
	if parent == abs {
		return abs, nil
	}
	realParent, err := resolveRealPath(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(realParent, filepath.Base(abs)), nil
}
