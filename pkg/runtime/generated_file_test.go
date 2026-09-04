package runtime

import (
	"context"
	"os"
	"path/filepath"
	stdruntime "runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

// resolverTestRuntime is a LocalRuntime over an in-memory store with one
// owning session that has a real workspace root.
func resolverTestRuntime(t *testing.T, sess *session.Session) (*LocalRuntime, session.Store) {
	t.Helper()
	store := session.NewInMemorySessionStore()
	require.NoError(t, store.AddSession(t.Context(), sess))
	return &LocalRuntime{sessionStore: store, now: time.Now}, store
}

// recordWorkspaceFile writes content at relPath under root and records it
// in the manifest, exactly as materialization would.
func recordWorkspaceFile(t *testing.T, store session.Store, sessID, root, relPath string, content []byte) {
	t.Helper()
	target := filepath.Join(root, filepath.FromSlash(relPath))
	require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o755))
	require.NoError(t, os.WriteFile(target, content, 0o644))
	require.NoError(t, manifestOf(t, store).AddGeneratedFile(t.Context(), session.GeneratedFile{
		SessionID: sessID,
		RelPath:   relPath,
		Root:      chat.ArtifactRootWorkspace,
		MimeType:  "image/png",
		CreatedAt: time.Now(),
	}))
}

func workspaceRef(owner, relPath string) GeneratedFileRef {
	return GeneratedFileRef{OwnerSessionID: owner, Root: chat.ArtifactRootWorkspace, Path: relPath}
}

func requireSymlinkSupport(t *testing.T) {
	t.Helper()
	if stdruntime.GOOS == "windows" {
		t.Skip("symlink creation requires elevated privileges on Windows")
	}
}

func TestResolveGeneratedFile_WorkspaceSuccess(t *testing.T) {
	t.Parallel()
	sess, root := workspaceSession(t, "sess-resolve")
	r, store := resolverTestRuntime(t, sess)
	recordWorkspaceFile(t, store, sess.ID, root, "images/cat.png", []byte("png-bytes"))

	resolved, err := r.ResolveGeneratedFile(t.Context(), workspaceRef(sess.ID, "images/cat.png"))

	require.NoError(t, err)
	assert.Equal(t, []byte("png-bytes"), resolved.Data)
	canonicalRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(canonicalRoot, "images", "cat.png"), resolved.Path)
}

// TestResolveGeneratedFile_ForgedReferenceRefused is the manifest contract:
// a reference alone — however session JSON was tampered with — must never
// select a real workspace file such as ".env" or a source file.
func TestResolveGeneratedFile_ForgedReferenceRefused(t *testing.T) {
	t.Parallel()
	sess, root := workspaceSession(t, "sess-forged")
	r, _ := resolverTestRuntime(t, sess)
	require.NoError(t, os.WriteFile(filepath.Join(root, ".env"), []byte("SECRET=1"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "main.go"), []byte("package main"), 0o644))

	for _, path := range []string{".env", "main.go"} {
		_, err := r.ResolveGeneratedFile(t.Context(), workspaceRef(sess.ID, path))
		assert.ErrorIs(t, err, ErrGeneratedFileUnavailable, "unrecorded workspace file %q must not resolve", path)
	}
}

func TestResolveGeneratedFile_LegacyAndInvalidRefsRefused(t *testing.T) {
	t.Parallel()
	sess, root := workspaceSession(t, "sess-legacy")
	r, store := resolverTestRuntime(t, sess)
	recordWorkspaceFile(t, store, sess.ID, root, "cat.png", []byte("png"))

	for name, ref := range map[string]GeneratedFileRef{
		"legacy empty root":          {OwnerSessionID: sess.ID, Root: "", Path: "cat.png"},
		"unknown root kind":          {OwnerSessionID: sess.ID, Root: "datadir", Path: "cat.png"},
		"missing owner":              {Root: chat.ArtifactRootWorkspace, Path: "cat.png"},
		"traversal path":             workspaceRef(sess.ID, "../cat.png"),
		"absolute path as workspace": workspaceRef(sess.ID, filepath.Join(root, "cat.png")),
	} {
		_, err := r.ResolveGeneratedFile(t.Context(), ref)
		assert.ErrorIs(t, err, ErrGeneratedFileUnavailable, name)
	}
}

// TestResolveGeneratedFile_RootKindMismatchRefused: a recorded workspace
// path must not resolve through an external-rooted reference (and vice
// versa) — the record's root kind is part of the trust decision.
func TestResolveGeneratedFile_RootKindMismatchRefused(t *testing.T) {
	t.Parallel()
	sess, root := workspaceSession(t, "sess-rootkind")
	r, store := resolverTestRuntime(t, sess)
	recordWorkspaceFile(t, store, sess.ID, root, "cat.png", []byte("png"))

	_, err := r.ResolveGeneratedFile(t.Context(), GeneratedFileRef{
		OwnerSessionID: sess.ID,
		Root:           chat.ArtifactRootExternal,
		Path:           "cat.png",
	})
	assert.ErrorIs(t, err, ErrGeneratedFileUnavailable)
}

func TestResolveGeneratedFile_MissingFileRefused(t *testing.T) {
	t.Parallel()
	sess, root := workspaceSession(t, "sess-missing")
	r, store := resolverTestRuntime(t, sess)
	recordWorkspaceFile(t, store, sess.ID, root, "cat.png", []byte("png"))
	require.NoError(t, os.Remove(filepath.Join(root, "cat.png")))

	_, err := r.ResolveGeneratedFile(t.Context(), workspaceRef(sess.ID, "cat.png"))
	assert.ErrorIs(t, err, ErrGeneratedFileUnavailable)
}

// TestResolveGeneratedFile_SymlinkReplacementRefused: a symlink swapped in
// after the manifest record must not be followed, even when its target is
// another file INSIDE the workspace.
func TestResolveGeneratedFile_SymlinkReplacementRefused(t *testing.T) {
	t.Parallel()
	requireSymlinkSupport(t)
	sess, root := workspaceSession(t, "sess-symlink")
	r, store := resolverTestRuntime(t, sess)
	recordWorkspaceFile(t, store, sess.ID, root, "cat.png", []byte("png"))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".env"), []byte("SECRET=1"), 0o600))
	require.NoError(t, os.Remove(filepath.Join(root, "cat.png")))
	require.NoError(t, os.Symlink(filepath.Join(root, ".env"), filepath.Join(root, "cat.png")))

	_, err := r.ResolveGeneratedFile(t.Context(), workspaceRef(sess.ID, "cat.png"))
	assert.ErrorIs(t, err, ErrGeneratedFileUnavailable)
}

// TestResolveGeneratedFile_SymlinkParentRefused: replacing a recorded
// path's parent directory with a symlink must equally refuse, even when
// the link stays inside the workspace.
func TestResolveGeneratedFile_SymlinkParentRefused(t *testing.T) {
	t.Parallel()
	requireSymlinkSupport(t)
	sess, root := workspaceSession(t, "sess-symlink-parent")
	r, store := resolverTestRuntime(t, sess)
	recordWorkspaceFile(t, store, sess.ID, root, "images/cat.png", []byte("png"))

	other := filepath.Join(root, "other")
	require.NoError(t, os.MkdirAll(other, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(other, "cat.png"), []byte("SECRET"), 0o644))
	require.NoError(t, os.RemoveAll(filepath.Join(root, "images")))
	require.NoError(t, os.Symlink(other, filepath.Join(root, "images")))

	_, err := r.ResolveGeneratedFile(t.Context(), workspaceRef(sess.ID, "images/cat.png"))
	assert.ErrorIs(t, err, ErrGeneratedFileUnavailable)
}

// TestResolveGeneratedFile_CrossWorkspaceOwner: resolution uses the OWNING
// session's persisted workspace, never any other session's (or the
// viewer's) directory — a same-named file elsewhere must not shadow it.
func TestResolveGeneratedFile_CrossWorkspaceOwner(t *testing.T) {
	t.Parallel()
	owner, ownerRoot := workspaceSession(t, "sess-owner")
	r, store := resolverTestRuntime(t, owner)
	recordWorkspaceFile(t, store, owner.ID, ownerRoot, "cat.png", []byte("owner-bytes"))

	otherRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(otherRoot, "cat.png"), []byte("other-bytes"), 0o644))
	require.NoError(t, store.AddSession(t.Context(), &session.Session{ID: "sess-viewer", WorkingDir: otherRoot}))

	resolved, err := r.ResolveGeneratedFile(t.Context(), workspaceRef(owner.ID, "cat.png"))

	require.NoError(t, err)
	assert.Equal(t, []byte("owner-bytes"), resolved.Data)
	canonicalRoot, err := filepath.EvalSymlinks(ownerRoot)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(canonicalRoot, "cat.png"), resolved.Path)
}

// TestResolveGeneratedFile_ParentWorkingDirFallback: an old sub-session
// without its own WorkingDir resolves through its parent's, mirroring
// session.ResolveWorkingDir.
func TestResolveGeneratedFile_ParentWorkingDirFallback(t *testing.T) {
	t.Parallel()
	parent, root := workspaceSession(t, "sess-parent")
	r, store := resolverTestRuntime(t, parent)
	child := &session.Session{ID: "sess-child", ParentID: parent.ID}
	require.NoError(t, store.AddSession(t.Context(), child))
	recordWorkspaceFile(t, store, child.ID, root, "cat.png", []byte("png"))

	resolved, err := r.ResolveGeneratedFile(t.Context(), workspaceRef(child.ID, "cat.png"))

	require.NoError(t, err)
	assert.Equal(t, []byte("png"), resolved.Data)
}

func TestResolveGeneratedFile_BlobWithoutManifestIsRefused(t *testing.T) {
	t.Parallel()
	sess, _ := workspaceSession(t, "sess-forged-blob")
	r, store := resolverTestRuntime(t, sess)
	require.NoError(t, store.(session.GeneratedMediaBlobStore).AddGeneratedBlob(t.Context(), sess.ID, ".env", []byte("SECRET")))

	_, err := r.ResolveGeneratedFile(t.Context(), workspaceRef(sess.ID, ".env"))
	assert.ErrorIs(t, err, ErrGeneratedFileUnavailable)
}

func TestResolveGeneratedFile_PortableBlobWinsOverWorkspace(t *testing.T) {
	t.Parallel()
	sess, root := workspaceSession(t, "sess-portable")

	r, store := resolverTestRuntime(t, sess)
	recordWorkspaceFile(t, store, sess.ID, root, "cat.png", []byte("workspace"))
	require.NoError(t, store.(session.GeneratedMediaBlobStore).AddGeneratedBlob(t.Context(), sess.ID, "cat.png", []byte("database")))
	require.NoError(t, os.Remove(filepath.Join(root, "cat.png")))

	resolved, err := r.ResolveGeneratedFile(t.Context(), workspaceRef(sess.ID, "cat.png"))
	require.NoError(t, err)
	assert.Equal(t, []byte("database"), resolved.Data)
}

func TestResolveGeneratedFile_LegacyManifestFallsBackToWorkspace(t *testing.T) {
	t.Parallel()
	sess, root := workspaceSession(t, "sess-legacy")
	r, store := resolverTestRuntime(t, sess)
	recordWorkspaceFile(t, store, sess.ID, root, "cat.png", []byte("legacy"))

	resolved, err := r.ResolveGeneratedFile(t.Context(), workspaceRef(sess.ID, "cat.png"))
	require.NoError(t, err)
	assert.Equal(t, []byte("legacy"), resolved.Data)
}

func TestResolveGeneratedFile_NoWorkspaceRootRefused(t *testing.T) {
	t.Parallel()
	sess := &session.Session{ID: "sess-rootless"}

	r, store := resolverTestRuntime(t, sess)
	require.NoError(t, manifestOf(t, store).AddGeneratedFile(t.Context(), session.GeneratedFile{
		SessionID: sess.ID, RelPath: "cat.png", Root: chat.ArtifactRootWorkspace, MimeType: "image/png", CreatedAt: time.Now(),
	}))

	_, err := r.ResolveGeneratedFile(t.Context(), workspaceRef(sess.ID, "cat.png"))
	assert.ErrorIs(t, err, ErrGeneratedFileUnavailable)
}

func TestResolveGeneratedFile_ExternalSuccessAndLeafSymlinkRefused(t *testing.T) {
	t.Parallel()
	requireSymlinkSupport(t)
	sess, _ := workspaceSession(t, "sess-external")
	r, store := resolverTestRuntime(t, sess)

	target := filepath.Join(t.TempDir(), "cat.png")
	require.NoError(t, os.WriteFile(target, []byte("external-bytes"), 0o644))
	require.NoError(t, manifestOf(t, store).AddGeneratedFile(t.Context(), session.GeneratedFile{
		SessionID: sess.ID, RelPath: target, Root: chat.ArtifactRootExternal, MimeType: "image/png", CreatedAt: time.Now(),
	}))
	ref := GeneratedFileRef{OwnerSessionID: sess.ID, Root: chat.ArtifactRootExternal, Path: target}

	resolved, err := r.ResolveGeneratedFile(t.Context(), ref)
	require.NoError(t, err)
	assert.Equal(t, []byte("external-bytes"), resolved.Data)
	assert.Equal(t, target, resolved.Path, "the external path the user confirmed is the display path")

	require.NoError(t, os.Remove(target))
	secret := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(secret, []byte("SECRET"), 0o600))
	require.NoError(t, os.Symlink(secret, target))
	_, err = r.ResolveGeneratedFile(t.Context(), ref)
	assert.ErrorIs(t, err, ErrGeneratedFileUnavailable, "an external leaf replaced by a symlink must be refused")
}

func TestResolveGeneratedFile_LegacyManifestRoundTripsLargeWorkspaceFile(t *testing.T) {
	t.Parallel()
	sess, root := workspaceSession(t, "sess-large-legacy")
	r, store := resolverTestRuntime(t, sess)
	recordWorkspaceFile(t, store, sess.ID, root, "cat.png", []byte("png"))
	path := filepath.Join(root, "cat.png")
	const size = (20 << 20) + 1
	require.NoError(t, os.Truncate(path, size))
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteAt([]byte{0x7f}, size-1)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	resolved, err := r.ResolveGeneratedFile(t.Context(), workspaceRef(sess.ID, "cat.png"))
	require.NoError(t, err)
	require.Len(t, resolved.Data, size)
	assert.Equal(t, byte(0x7f), resolved.Data[size-1])
}

// countingStore wraps a session store to observe how often resolution hits
// the store, proving the per-session cache.
type countingStore struct {
	session.Store

	getSessions int
	lookups     int
}

func (s *countingStore) GetSession(ctx context.Context, id string) (*session.Session, error) {
	s.getSessions++
	return s.Store.GetSession(ctx, id)
}

func (s *countingStore) LookupGeneratedFile(ctx context.Context, sessionID, relPath string) (*session.GeneratedFile, error) {
	s.lookups++
	return s.Store.(session.GeneratedMediaManifest).LookupGeneratedFile(ctx, sessionID, relPath)
}

func (s *countingStore) AddGeneratedFile(ctx context.Context, file session.GeneratedFile) error {
	return s.Store.(session.GeneratedMediaManifest).AddGeneratedFile(ctx, file)
}

func TestResolveGeneratedFile_CachesRootAndManifestPerSession(t *testing.T) {
	t.Parallel()
	sess, root := workspaceSession(t, "sess-cache")
	inner := session.NewInMemorySessionStore()
	require.NoError(t, inner.AddSession(t.Context(), sess))
	store := &countingStore{Store: inner}
	r := &LocalRuntime{sessionStore: store, now: time.Now}
	recordWorkspaceFile(t, store, sess.ID, root, "cat.png", []byte("png"))

	for range 3 {
		_, err := r.ResolveGeneratedFile(t.Context(), workspaceRef(sess.ID, "cat.png"))
		require.NoError(t, err)
	}

	assert.Equal(t, 1, store.lookups, "repeat resolutions must reuse the cached manifest record")
	assert.Equal(t, 1, store.getSessions, "repeat resolutions must reuse the cached workspace root")
}

// TestResolveGeneratedFile_RecordSeedsAndRefreshesCache: materialization's
// recordGeneratedFile both seeds the cache (no store reads on first
// resolve) and refreshes it when the same path is re-recorded — a stale
// entry must not outlive an upsert.
func TestResolveGeneratedFile_RecordSeedsAndRefreshesCache(t *testing.T) {
	t.Parallel()
	sess, root := workspaceSession(t, "sess-cache-seed")
	inner := session.NewInMemorySessionStore()
	require.NoError(t, inner.AddSession(t.Context(), sess))
	store := &countingStore{Store: inner}
	r := &LocalRuntime{sessionStore: store, now: time.Now}

	require.NoError(t, os.WriteFile(filepath.Join(root, "cat.png"), []byte("png"), 0o644))
	require.NoError(t, r.recordGeneratedFile(t.Context(), sess.ID, chat.ArtifactRootWorkspace, "cat.png", "image/png"))
	r.generatedFiles.setRoot(sess.ID, root) // materializeGeneratedMedia seeds this alongside

	_, err := r.ResolveGeneratedFile(t.Context(), workspaceRef(sess.ID, "cat.png"))
	require.NoError(t, err)
	assert.Zero(t, store.lookups, "a freshly recorded file must resolve without a manifest read")
	assert.Zero(t, store.getSessions, "a freshly recorded file must resolve without a session read")

	// Re-record the same path as external: the cached record must follow,
	// so the old workspace-rooted reference stops resolving.
	external := filepath.Join(t.TempDir(), "cat.png")
	require.NoError(t, os.WriteFile(external, []byte("ext"), 0o644))
	require.NoError(t, r.recordGeneratedFile(t.Context(), sess.ID, chat.ArtifactRootExternal, external, "image/png"))
	resolved, err := r.ResolveGeneratedFile(t.Context(), GeneratedFileRef{
		OwnerSessionID: sess.ID, Root: chat.ArtifactRootExternal, Path: external,
	})
	require.NoError(t, err)
	assert.Equal(t, []byte("ext"), resolved.Data)
}

// TestResolveGeneratedFile_MaterializedEndToEnd drives the real
// materialization path and resolves the reference it persisted — the
// exact live TUI flow.
func TestResolveGeneratedFile_MaterializedEndToEnd(t *testing.T) {
	r, store, _ := newMediaTestRuntime(t)
	sess, root := workspaceSession(t, "sess-e2e")
	require.NoError(t, store.AddSession(t.Context(), sess))

	parts := r.materializeGeneratedMedia(t.Context(), sess, []chat.MediaDelta{
		{Data: []byte{0xAA}, MimeType: "image/png", Name: "cat.png", Size: 1},
	}, "root", nil)
	require.Len(t, parts, 1)
	src := parts[0].Document.Source

	resolved, err := r.ResolveGeneratedFile(t.Context(), GeneratedFileRef{
		OwnerSessionID: src.ArtifactOwnerSessionID,
		Root:           src.ArtifactRoot,
		Path:           src.ArtifactPath,
	})

	require.NoError(t, err)
	assert.Equal(t, []byte{0xAA}, resolved.Data)
	canonicalRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(canonicalRoot, "cat.png"), resolved.Path)
}
