package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrWorkingDirUnavailable is returned by ResolveWorkingDir when neither the
// session nor any ancestor carries a usable workspace root. Callers must
// treat it as "no workspace" — never substitute the viewer's process cwd,
// which could belong to a different workspace than the one that owned the
// session's files.
var ErrWorkingDirUnavailable = errors.New("session working directory unavailable")

// maxWorkingDirAncestry bounds how many sessions (including the starting one)
// ResolveWorkingDir inspects while walking parent links, protecting against
// corrupt stores with very deep or cyclic parent chains.
const maxWorkingDirAncestry = 32

// Lookup loads a session by ID. Store.GetSession satisfies it; tests can
// supply a map-backed stub.
type Lookup func(ctx context.Context, id string) (*Session, error)

// CaptureLocalWorkingDir returns the absolute effective workspace root for a
// session being created on a surface with a LOCAL workspace. configured is
// the surface's configured working directory (e.g. --working-dir); when it is
// empty the process cwd — captured once, at creation/startup time — is the
// workspace. Remote or headless surfaces without a local workspace must not
// call this: their sessions intentionally keep an empty WorkingDir.
func CaptureLocalWorkingDir(configured string) (string, error) {
	configured = strings.TrimSpace(configured)
	if configured == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("capturing workspace root: %w", err)
		}
		return filepath.Clean(cwd), nil
	}
	abs, err := filepath.Abs(configured)
	if err != nil {
		return "", fmt.Errorf("capturing workspace root: %w", err)
	}
	return filepath.Clean(abs), nil
}

// ResolveWorkingDir returns the workspace root that owns sess. It prefers the
// session's own persisted WorkingDir and, for old persisted sub-sessions that
// predate creation-time provenance, walks the parent chain (bounded, cycle
// safe) until an ancestor provides one. Stored values must be absolute and
// well-formed; anything else fails with ErrWorkingDirUnavailable rather than
// being resolved against the current process cwd. The workspace directory is
// not required to still exist — a deleted workspace is still valid provenance.
func ResolveWorkingDir(ctx context.Context, sess *Session, lookup Lookup) (string, error) {
	if sess == nil {
		return "", fmt.Errorf("%w: nil session", ErrWorkingDirUnavailable)
	}

	visited := make(map[string]struct{}, 4)
	for range maxWorkingDirAncestry {
		if sess.WorkingDir != "" {
			if err := validateStoredWorkingDir(sess.WorkingDir); err != nil {
				return "", fmt.Errorf("%w: session %s: %w", ErrWorkingDirUnavailable, sess.ID, err)
			}
			return sess.WorkingDir, nil
		}
		if sess.ParentID == "" {
			return "", fmt.Errorf("%w: session %s has no workspace root and no parent", ErrWorkingDirUnavailable, sess.ID)
		}
		if sess.ID != "" {
			visited[sess.ID] = struct{}{}
		}
		if _, seen := visited[sess.ParentID]; seen {
			return "", fmt.Errorf("%w: parent chain cycle at session %s", ErrWorkingDirUnavailable, sess.ParentID)
		}
		if lookup == nil {
			return "", fmt.Errorf("%w: session %s needs parent lookup but none was provided", ErrWorkingDirUnavailable, sess.ID)
		}
		parent, err := lookup(ctx, sess.ParentID)
		if err != nil {
			return "", fmt.Errorf("%w: loading parent %s: %w", ErrWorkingDirUnavailable, sess.ParentID, err)
		}
		if parent == nil || parent.ID != sess.ParentID {
			return "", fmt.Errorf("%w: lookup for parent %s returned a different session", ErrWorkingDirUnavailable, sess.ParentID)
		}
		sess = parent
	}
	return "", fmt.Errorf("%w: parent chain exceeds %d sessions", ErrWorkingDirUnavailable, maxWorkingDirAncestry)
}

// validateStoredWorkingDir vets a persisted WorkingDir before it is trusted as
// a workspace root. It is strict on purpose: a malformed value must make
// resolution fail closed instead of being repaired relative to whatever
// directory the viewing process happens to run in.
func validateStoredWorkingDir(dir string) error {
	if dir != strings.TrimSpace(dir) {
		return errors.New("working directory has surrounding whitespace")
	}
	if strings.ContainsRune(dir, 0) {
		return errors.New("working directory contains a NUL byte")
	}
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("working directory %q is not absolute", dir)
	}
	// Every legitimate writer runs filepath.Clean before persisting (see
	// CaptureLocalWorkingDir), so an unclean stored value — ".." segments,
	// "." segments, doubled or trailing separators — is tampered or corrupt.
	// Rejecting it here keeps traversal like "/workspace/../etc" from ever
	// being handed out as a trusted workspace root.
	if filepath.Clean(dir) != dir {
		return fmt.Errorf("working directory %q is not a clean path", dir)
	}
	return nil
}
