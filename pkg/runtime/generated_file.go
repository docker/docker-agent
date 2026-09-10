package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

// ErrGeneratedFileUnavailable is the single caller-visible failure of
// [LocalRuntime.ResolveGeneratedFile]. Every refusal — unknown root kind,
// missing manifest record, root-kind mismatch, workspace escape, symlink
// replacement, or missing file — collapses into it so UIs can
// only ever say "unavailable"; the wrapped cause is for debug logs.
var ErrGeneratedFileUnavailable = errors.New("generated file unavailable")

// GeneratedFileRef identifies one persisted generated-media reference, as
// carried by [chat.DocumentSource] (ArtifactPath/ArtifactRoot/
// ArtifactOwnerSessionID).
type GeneratedFileRef struct {
	// OwnerSessionID is the session the file was materialized under — the
	// owning session, never the viewing one.
	OwnerSessionID string
	// Root is the root kind Path is interpreted against. Only
	// chat.ArtifactRootWorkspace resolves; all other kinds are unavailable.
	Root chat.ArtifactRootKind
	// Path is the recorded workspace-relative slash-separated final path.
	Path string
}

// ResolvedGeneratedFile carries the resolved bytes and a display path.
// Portable blobs remain readable without workspace provenance; in that case
// Path is the recorded relative artifact path, not a canonical absolute path.
type ResolvedGeneratedFile struct {
	Data []byte
	Path string
}

// generatedFileCache keeps resolved workspace provenance per owner session.
// Manifest authorization is deliberately not cached: every resolution must
// observe current store state so deleting a session immediately revokes its
// generated-file references.
type generatedFileCache struct {
	mu    sync.Mutex
	roots map[string]string // owner session ID → workspace root
}

func (c *generatedFileCache) root(ownerID string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	root, ok := c.roots[ownerID]
	return root, ok
}

func (c *generatedFileCache) setRoot(ownerID, root string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.roots == nil {
		c.roots = make(map[string]string)
	}
	c.roots[ownerID] = root
}

// ResolveGeneratedFile resolves one recorded generated-media reference to
// its bytes and validated canonical path. It is the only supported read
// path for generated media: the (owner session, path) pair must have been
// recorded in the generated-media manifest by materialization, the root
// kind must match the record, and a workspace path must still be a plain
// regular file inside the owning session's workspace — a reference alone,
// however it was forged, never selects a file.
//
// It is safe for concurrent use and intended to be called off the UI
// update loop (e.g. inside a tea.Cmd).
func (r *LocalRuntime) ResolveGeneratedFile(ctx context.Context, ref GeneratedFileRef) (*ResolvedGeneratedFile, error) {
	if ref.OwnerSessionID == "" {
		return nil, fmt.Errorf("%w: reference without an owner session", ErrGeneratedFileUnavailable)
	}
	if ref.Root != chat.ArtifactRootWorkspace {
		return nil, fmt.Errorf("%w: unresolvable root kind %q", ErrGeneratedFileUnavailable, ref.Root)
	}

	if !fs.ValidPath(ref.Path) || strings.ContainsAny(ref.Path, "\\:\x00\r\n") || ref.Path == "." || strings.HasPrefix(ref.Path, "~") {
		return nil, fmt.Errorf("%w: invalid workspace path", ErrGeneratedFileUnavailable)
	}

	record, err := r.lookupGeneratedFile(ctx, ref)
	if err != nil {
		return nil, err
	}
	if record.Root != ref.Root {
		return nil, fmt.Errorf("%w: reference root %q does not match recorded root %q", ErrGeneratedFileUnavailable, ref.Root, record.Root)
	}

	if blobs, ok := r.sessionStore.(session.GeneratedMediaBlobStore); ok {
		data, err := blobs.LookupGeneratedBlob(ctx, ref.OwnerSessionID, ref.Path)
		if err == nil {
			return &ResolvedGeneratedFile{Data: data, Path: generatedFileDisplayPath(ctx, r, ref)}, nil
		}
		if !errors.Is(err, session.ErrGeneratedBlobNotFound) {
			return nil, fmt.Errorf("%w: loading portable media: %w", ErrGeneratedFileUnavailable, err)
		}
	}

	workspaceRoot, err := r.generatedFileWorkspaceRoot(ctx, ref.OwnerSessionID)
	if err != nil {
		return nil, err
	}
	data, canonical, err := readWorkspaceGeneratedFile(workspaceRoot, ref.Path)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrGeneratedFileUnavailable, err)
	}
	return &ResolvedGeneratedFile{Data: data, Path: canonical}, nil
}

func generatedFileDisplayPath(ctx context.Context, r *LocalRuntime, ref GeneratedFileRef) string {
	root, err := r.generatedFileWorkspaceRoot(ctx, ref.OwnerSessionID)
	if err != nil {
		return ref.Path
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		canonicalRoot = root
	}
	return filepath.Join(canonicalRoot, filepath.FromSlash(ref.Path))
}

// lookupGeneratedFile returns the current manifest record for ref.
func (r *LocalRuntime) lookupGeneratedFile(ctx context.Context, ref GeneratedFileRef) (session.GeneratedFile, error) {
	manifest, ok := r.sessionStore.(session.GeneratedMediaManifest)
	if !ok {
		return session.GeneratedFile{}, fmt.Errorf("%w: session store %T has no generated-media manifest", ErrGeneratedFileUnavailable, r.sessionStore)
	}
	record, err := manifest.LookupGeneratedFile(ctx, ref.OwnerSessionID, ref.Path)
	if err != nil {
		return session.GeneratedFile{}, fmt.Errorf("%w: %w", ErrGeneratedFileUnavailable, err)
	}
	return *record, nil
}

// generatedFileWorkspaceRoot returns the OWNING session's workspace root —
// persisted WorkingDir with the bounded parent-chain fallback, never the
// viewer's cwd — from the cache or the session store.
func (r *LocalRuntime) generatedFileWorkspaceRoot(ctx context.Context, ownerID string) (string, error) {
	if root, ok := r.generatedFiles.root(ownerID); ok {
		return root, nil
	}
	if r.sessionStore == nil {
		return "", fmt.Errorf("%w: no session store to resolve the owner workspace", ErrGeneratedFileUnavailable)
	}
	owner, err := r.sessionStore.GetSession(ctx, ownerID)
	if err != nil {
		return "", fmt.Errorf("%w: loading owner session: %w", ErrGeneratedFileUnavailable, err)
	}
	root, err := session.ResolveWorkingDir(ctx, owner, r.sessionLookup())
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrGeneratedFileUnavailable, err)
	}
	r.generatedFiles.setRoot(ownerID, root)
	return root, nil
}

// readWorkspaceGeneratedFile reads relPath under workspaceRoot with the
// same containment the writer enforced: os.Root confines every operation
// to the workspace, and no path component may be a symlink — the manifest
// recorded a regular file written by pkg/workspacemedia, so a symlink
// found now (even one pointing elsewhere INSIDE the workspace, e.g. at
// ".env") means the file was replaced and must not be followed.
func readWorkspaceGeneratedFile(workspaceRoot, relPath string) (data []byte, canonical string, err error) {
	root, err := os.OpenRoot(workspaceRoot)
	if err != nil {
		return nil, "", fmt.Errorf("opening workspace root: %w", err)
	}
	defer root.Close()

	osRel := filepath.FromSlash(relPath)
	if err := rejectSymlinkComponents(root, relPath); err != nil {
		return nil, "", err
	}

	f, err := root.Open(osRel)
	if err != nil {
		return nil, "", fmt.Errorf("opening recorded file: %w", err)
	}
	defer f.Close()
	data, err = readRegularGeneratedFile(f, func() (os.FileInfo, error) { return root.Lstat(osRel) })
	if err != nil {
		return nil, "", err
	}

	// The workspace root itself may legitimately be reached through
	// symlinks (e.g. macOS /tmp); canonicalize it so the displayed path is
	// the real location. The recorded relative path below it is
	// symlink-free (checked above), so a plain join stays canonical.
	canonicalRoot, err := filepath.EvalSymlinks(workspaceRoot)
	if err != nil {
		canonicalRoot = workspaceRoot
	}
	return data, filepath.Join(canonicalRoot, osRel), nil
}

// rejectSymlinkComponents fails when any component of the slash-separated
// relPath — intermediate directory or final file — is a symlink inside
// root.
func rejectSymlinkComponents(root *os.Root, relPath string) error {
	components := strings.Split(relPath, "/")
	for i := range components {
		prefix := path.Join(components[:i+1]...)
		fi, err := root.Lstat(filepath.FromSlash(prefix))
		if err != nil {
			return fmt.Errorf("inspecting recorded path: %w", err)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("recorded path component %q was replaced by a symlink", prefix)
		}
		if i < len(components)-1 && !fi.IsDir() {
			return fmt.Errorf("recorded path component %q is not a directory", prefix)
		}
	}
	return nil
}

// readRegularGeneratedFile reads an opened generated file after verifying —
// against a fresh Lstat taken AFTER the open, closing the check/open race —
// that the path still names this exact regular file rather than a symlink
// swapped in since materialization.
func readRegularGeneratedFile(f *os.File, lstat func() (os.FileInfo, error)) ([]byte, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspecting recorded file: %w", err)
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("recorded file is not a regular file (%s)", st.Mode())
	}
	lfi, err := lstat()
	if err != nil {
		return nil, fmt.Errorf("re-inspecting recorded path: %w", err)
	}
	if lfi.Mode()&os.ModeSymlink != 0 || !os.SameFile(lfi, st) {
		return nil, errors.New("recorded path no longer names the opened file")
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("reading recorded file: %w", err)
	}
	return data, nil
}
