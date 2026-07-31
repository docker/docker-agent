package session

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
// validation on BOTH write and lookup: shapes workspacemedia.Write can never
// return (absolute, traversal, backslashes, NUL, empty/dot segments) fail
// with ErrInvalidGeneratedFilePath before touching storage.
func TestGeneratedMediaManifest_RejectsInvalidPaths(t *testing.T) {
	t.Parallel()
	invalid := []string{
		"",
		"/etc/passwd",
		"/abs/cat.png",
		"../outside.png",
		"images/../../outside.png",
		"images/./cat.png",
		"images//cat.png",
		"images/cat.png/",
		`images\cat.png`,
		"cat\x00.png",
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
