package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	// chat.ArtifactRootWorkspace and chat.ArtifactRootExternal resolve;
	// the empty (unknown) kind is always unavailable.
	Root chat.ArtifactRootKind
	// Path is the recorded final path: workspace-relative slash-separated
	// for the workspace root, absolute for the external root.
	Path string
}

// ResolvedGeneratedFile is a successful resolution: the file bytes plus the
// validated canonical absolute path, safe to display verbatim (owner IDs,
// raw refs, and error details are never part of it).
type ResolvedGeneratedFile struct {
	Data []byte
	Path string
}

// generatedFileCache is the per-owner-session state ResolveGeneratedFile
// needs: the resolved workspace root and the manifest records seen so far.
// It is seeded by materialization (so live rendering does no store reads)
// and filled lazily for restored sessions. Only positive results are
// cached: a miss may become a hit when materialization records a new file,
// while recorded state never silently disappears — this runtime's store is
// the only manifest writer, and an upsert for an existing path refreshes
// the entry through cacheGeneratedFile.
type generatedFileCache struct {
	mu    sync.Mutex
	roots map[string]string                           // owner session ID → workspace root
	files map[string]map[string]session.GeneratedFile // owner session ID → recorded path → record
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

func (c *generatedFileCache) file(ownerID, relPath string) (session.GeneratedFile, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	record, ok := c.files[ownerID][relPath]
	return record, ok
}

func (c *generatedFileCache) setFile(record session.GeneratedFile) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.files == nil {
		c.files = make(map[string]map[string]session.GeneratedFile)
	}
	if c.files[record.SessionID] == nil {
		c.files[record.SessionID] = make(map[string]session.GeneratedFile)
	}
	c.files[record.SessionID][record.RelPath] = record
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
	if ref.Root != chat.ArtifactRootWorkspace && ref.Root != chat.ArtifactRootExternal {
		return nil, fmt.Errorf("%w: unresolvable root kind %q", ErrGeneratedFileUnavailable, ref.Root)
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

	if ref.Root == chat.ArtifactRootExternal {
		data, err := readExternalGeneratedFile(ref.Path)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrGeneratedFileUnavailable, err)
		}
		return &ResolvedGeneratedFile{Data: data, Path: ref.Path}, nil
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
	if ref.Root == chat.ArtifactRootExternal {
		return ref.Path
	}
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

// lookupGeneratedFile returns the manifest record for ref, from the cache
// or the session store.
func (r *LocalRuntime) lookupGeneratedFile(ctx context.Context, ref GeneratedFileRef) (session.GeneratedFile, error) {
	if record, ok := r.generatedFiles.file(ref.OwnerSessionID, ref.Path); ok {
		return record, nil
	}
	manifest, ok := r.sessionStore.(session.GeneratedMediaManifest)
	if !ok {
		return session.GeneratedFile{}, fmt.Errorf("%w: session store %T has no generated-media manifest", ErrGeneratedFileUnavailable, r.sessionStore)
	}
	record, err := manifest.LookupGeneratedFile(ctx, ref.OwnerSessionID, ref.Path)
	if err != nil {
		return session.GeneratedFile{}, fmt.Errorf("%w: %w", ErrGeneratedFileUnavailable, err)
	}
	r.generatedFiles.setFile(*record)
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

// readExternalGeneratedFile reads a user-confirmed absolute external
// target. Parent directories may legitimately be symlinks (the user
// confirmed this exact path, e.g. under macOS /tmp), but the leaf must
// still be the regular file the writer created — a symlink swapped in
// afterwards is refused.
func readExternalGeneratedFile(target string) ([]byte, error) {
	f, err := os.Open(target)
	if err != nil {
		return nil, fmt.Errorf("opening recorded external file: %w", err)
	}
	defer f.Close()
	return readRegularGeneratedFile(f, func() (os.FileInfo, error) { return os.Lstat(target) })
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
