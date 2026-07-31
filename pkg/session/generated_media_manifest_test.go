package session

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
)

// manifestStores runs a subtest against both built-in Store implementations,
// which must expose identical GeneratedMediaManifest semantics.
func manifestStores(t *testing.T, run func(t *testing.T, store Store, manifest GeneratedMediaManifest)) {
	t.Helper()
	t.Run("in-memory", func(t *testing.T) {
		t.Parallel()
		store := NewInMemorySessionStore()
		run(t, store, store.(*InMemorySessionStore))
	})
	t.Run("sqlite", func(t *testing.T) {
		t.Parallel()
		store := openMemoryStore(t)
		run(t, store, store)
	})
}

func TestGeneratedMediaManifest_RoundTrip(t *testing.T) {
	t.Parallel()
	manifestStores(t, func(t *testing.T, _ Store, manifest GeneratedMediaManifest) {
		t.Helper()
		created := time.Date(2026, 7, 31, 12, 30, 45, 0, time.UTC)
		require.NoError(t, manifest.AddGeneratedFile(t.Context(), GeneratedFile{
			SessionID: "owner",
			RelPath:   "images/cat.png",
			MimeType:  "image/png",
			CreatedAt: created,
		}))

		got, err := manifest.LookupGeneratedFile(t.Context(), "owner", "images/cat.png")
		require.NoError(t, err)
		assert.Equal(t, "owner", got.SessionID)
		assert.Equal(t, "images/cat.png", got.RelPath)
		assert.Equal(t, "image/png", got.MimeType)
		assert.WithinDuration(t, created, got.CreatedAt, time.Second)
	})
}

// TestGeneratedMediaManifest_RefusesUnrecordedPaths is the trust-anchor
// contract: a workspace path materialization never wrote — a tampered
// session JSON pointing at ".env" or a source file — must be refused with
// the stable not-found error, never resolved by path shape alone.
func TestGeneratedMediaManifest_RefusesUnrecordedPaths(t *testing.T) {
	t.Parallel()
	manifestStores(t, func(t *testing.T, _ Store, manifest GeneratedMediaManifest) {
		t.Helper()
		require.NoError(t, manifest.AddGeneratedFile(t.Context(), GeneratedFile{
			SessionID: "owner", RelPath: "cat.png", MimeType: "image/png", CreatedAt: time.Now(),
		}))

		for _, relPath := range []string{".env", "src/main.go", "cat.jpg", "images/cat.png"} {
			_, err := manifest.LookupGeneratedFile(t.Context(), "owner", relPath)
			require.ErrorIs(t, err, ErrGeneratedFileNotFound, "unrecorded path %q must be refused", relPath)
		}

		// Cross-session isolation: another session never recorded cat.png.
		_, err := manifest.LookupGeneratedFile(t.Context(), "other-session", "cat.png")
		require.ErrorIs(t, err, ErrGeneratedFileNotFound)
	})
}

// TestGeneratedMediaManifest_RejectsInvalidPaths pins the API-boundary
// validation on BOTH write and lookup: shapes pkg/workspacemedia can never
// return (traversal, backslashes in relative paths, NUL, empty/dot
// segments, unclean absolutes) fail with ErrInvalidGeneratedFilePath before
// touching storage.
func TestGeneratedMediaManifest_RejectsInvalidPaths(t *testing.T) {
	t.Parallel()
	invalid := []string{
		"",
		"../outside.png",
		"images/../../outside.png",
		"images/./cat.png",
		"images//cat.png",
		"images/cat.png/",
		`images\cat.png`,
		"cat\x00.png",
		"/abs\x00/cat.png",
		"/abs/../cat.png",
		".",
		"..",
	}
	manifestStores(t, func(t *testing.T, _ Store, manifest GeneratedMediaManifest) {
		t.Helper()
		for _, relPath := range invalid {
			err := manifest.AddGeneratedFile(t.Context(), GeneratedFile{
				SessionID: "owner", RelPath: relPath, MimeType: "image/png", CreatedAt: time.Now(),
			})
			require.ErrorIs(t, err, ErrInvalidGeneratedFilePath, "add of %q must be rejected", relPath)

			_, err = manifest.LookupGeneratedFile(t.Context(), "owner", relPath)
			require.ErrorIs(t, err, ErrInvalidGeneratedFilePath, "lookup of %q must be rejected", relPath)
		}

		err := manifest.AddGeneratedFile(t.Context(), GeneratedFile{RelPath: "cat.png", MimeType: "image/png"})
		require.ErrorIs(t, err, ErrEmptyID, "an empty owner session ID must be rejected")
		_, err = manifest.LookupGeneratedFile(t.Context(), "", "cat.png")
		require.ErrorIs(t, err, ErrEmptyID)
	})
}

// TestGeneratedMediaManifest_ExternalRoundTrip: a user-confirmed external
// record stores the absolute confirmed path with the external root kind and
// returns both on lookup, so a resolver can require root agreement.
func TestGeneratedMediaManifest_ExternalRoundTrip(t *testing.T) {
	t.Parallel()
	manifestStores(t, func(t *testing.T, _ Store, manifest GeneratedMediaManifest) {
		t.Helper()
		externalPath := filepath.Join(t.TempDir(), "exports", "cat.png")
		require.NoError(t, manifest.AddGeneratedFile(t.Context(), GeneratedFile{
			SessionID: "owner",
			RelPath:   externalPath,
			Root:      chat.ArtifactRootExternal,
			MimeType:  "image/png",
			CreatedAt: time.Now(),
		}))

		got, err := manifest.LookupGeneratedFile(t.Context(), "owner", externalPath)
		require.NoError(t, err)
		assert.Equal(t, chat.ArtifactRootExternal, got.Root)
		assert.Equal(t, externalPath, got.RelPath)
	})
}

// TestGeneratedMediaManifest_WorkspaceRootNormalized: pre-external records
// (empty root kind) keep their workspace meaning on read-back.
func TestGeneratedMediaManifest_WorkspaceRootNormalized(t *testing.T) {
	t.Parallel()
	manifestStores(t, func(t *testing.T, _ Store, manifest GeneratedMediaManifest) {
		t.Helper()
		require.NoError(t, manifest.AddGeneratedFile(t.Context(), GeneratedFile{
			SessionID: "owner", RelPath: "cat.png", MimeType: "image/png", CreatedAt: time.Now(),
		}))
		got, err := manifest.LookupGeneratedFile(t.Context(), "owner", "cat.png")
		require.NoError(t, err)
		assert.Equal(t, chat.ArtifactRootWorkspace, got.Root)
	})
}

// TestGeneratedMediaManifest_RejectsMisRootedRecords: the add boundary
// refuses any (root, path) combination pkg/workspacemedia could never have
// produced, and unrecorded absolute paths still fail closed on lookup — a
// tampered session JSON cannot probe /etc/passwd through the manifest.
func TestGeneratedMediaManifest_RejectsMisRootedRecords(t *testing.T) {
	t.Parallel()
	manifestStores(t, func(t *testing.T, _ Store, manifest GeneratedMediaManifest) {
		t.Helper()
		absolutePath := filepath.Join(t.TempDir(), "abs", "cat.png")
		uncleanAbsolutePath := filepath.Dir(absolutePath) + string(filepath.Separator) + ".." + string(filepath.Separator) + filepath.Base(absolutePath)
		add := func(root chat.ArtifactRootKind, p string) error {
			return manifest.AddGeneratedFile(t.Context(), GeneratedFile{
				SessionID: "owner", RelPath: p, Root: root, MimeType: "image/png", CreatedAt: time.Now(),
			})
		}
		require.ErrorIs(t, add("", absolutePath), ErrInvalidGeneratedFilePath,
			"a workspace record must never carry an absolute path")
		require.ErrorIs(t, add(chat.ArtifactRootWorkspace, absolutePath), ErrInvalidGeneratedFilePath)
		require.ErrorIs(t, add(chat.ArtifactRootExternal, "cat.png"), ErrInvalidGeneratedFilePath,
			"an external record must carry an absolute path")
		require.ErrorIs(t, add(chat.ArtifactRootExternal, uncleanAbsolutePath), ErrInvalidGeneratedFilePath,
			"an external record must carry a clean path")
		require.ErrorIs(t, add("attacker-root", absolutePath), ErrInvalidGeneratedFileRoot)

		_, err := manifest.LookupGeneratedFile(t.Context(), "owner", absolutePath)
		require.ErrorIs(t, err, ErrGeneratedFileNotFound, "an unrecorded absolute path must be refused")
	})
}

// TestGeneratedMediaManifest_DeleteSessionPrunesRecords: the manifest table
// has no foreign key (the session row may not exist yet when materialization
// records a file), so DeleteSession must prune records explicitly.
func TestGeneratedMediaManifest_DeleteSessionPrunesRecords(t *testing.T) {
	t.Parallel()
	manifestStores(t, func(t *testing.T, store Store, manifest GeneratedMediaManifest) {
		t.Helper()
		ctx := t.Context()
		require.NoError(t, store.AddSession(ctx, New(WithID("doomed"))))
		require.NoError(t, store.AddSession(ctx, New(WithID("kept"))))
		require.NoError(t, manifest.AddGeneratedFile(ctx, GeneratedFile{
			SessionID: "doomed", RelPath: "cat.png", MimeType: "image/png", CreatedAt: time.Now(),
		}))
		require.NoError(t, manifest.AddGeneratedFile(ctx, GeneratedFile{
			SessionID: "kept", RelPath: "dog.png", MimeType: "image/png", CreatedAt: time.Now(),
		}))

		require.NoError(t, store.DeleteSession(ctx, "doomed"))

		_, err := manifest.LookupGeneratedFile(ctx, "doomed", "cat.png")
		require.ErrorIs(t, err, ErrGeneratedFileNotFound, "deleting a session must prune its manifest records")
		_, err = manifest.LookupGeneratedFile(ctx, "kept", "dog.png")
		assert.NoError(t, err, "another session's records must survive")
	})
}

// TestGeneratedMediaManifest_RecordBeforeSessionRow: materialization may run
// before the lazily persisted session row exists — the manifest must accept
// the record anyway (this is why the table carries no foreign key).
func TestGeneratedMediaManifest_RecordBeforeSessionRow(t *testing.T) {
	t.Parallel()
	manifestStores(t, func(t *testing.T, _ Store, manifest GeneratedMediaManifest) {
		t.Helper()
		require.NoError(t, manifest.AddGeneratedFile(t.Context(), GeneratedFile{
			SessionID: "not-yet-persisted", RelPath: "cat.png", MimeType: "image/png", CreatedAt: time.Now(),
		}))
		_, err := manifest.LookupGeneratedFile(t.Context(), "not-yet-persisted", "cat.png")
		assert.NoError(t, err)
	})
}

// TestGeneratedMediaManifest_ReAddUpdatesRecord: re-recording the same
// (session, path) key — e.g. a retried materialization — keeps a single
// record carrying the latest MIME and timestamp.
func TestGeneratedMediaManifest_ReAddUpdatesRecord(t *testing.T) {
	t.Parallel()
	manifestStores(t, func(t *testing.T, _ Store, manifest GeneratedMediaManifest) {
		t.Helper()
		ctx := t.Context()
		require.NoError(t, manifest.AddGeneratedFile(ctx, GeneratedFile{
			SessionID: "owner", RelPath: "cat.png", MimeType: "image/png", CreatedAt: time.Now().Add(-time.Hour),
		}))
		require.NoError(t, manifest.AddGeneratedFile(ctx, GeneratedFile{
			SessionID: "owner", RelPath: "cat.png", MimeType: "image/webp", CreatedAt: time.Now(),
		}))

		got, err := manifest.LookupGeneratedFile(ctx, "owner", "cat.png")
		require.NoError(t, err)
		assert.Equal(t, "image/webp", got.MimeType)
	})
}
