package session

import (
	"context"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeleteSessionRemovesSubtreeAndStoredMedia(t *testing.T) {
	t.Parallel()
	manifestStores(t, func(t *testing.T, store Store, manifest GeneratedMediaManifest) {
		t.Helper()
		ctx := t.Context()
		if sqlite, ok := store.(*SQLiteSessionStore); ok {
			_, err := sqlite.db.ExecContext(ctx, "PRAGMA foreign_keys = ON")
			require.NoError(t, err)
		}
		for _, entry := range []struct{ id, parent string }{{"root", ""}, {"child", "root"}, {"grandchild", "child"}, {"kept", ""}} {
			require.NoError(t, store.AddSession(ctx, New(WithID(entry.id), WithParentID(entry.parent), WithUserMessage("history"))))
			require.NoError(t, manifest.AddGeneratedFile(ctx, GeneratedFile{SessionID: entry.id, RelPath: "image.png", MimeType: "image/png"}))
			require.NoError(t, manifest.(GeneratedMediaBlobStore).AddGeneratedBlob(ctx, entry.id, "image.png", []byte("bytes")))
		}
		require.NoError(t, store.DeleteSession(ctx, "root"))
		for _, id := range []string{"root", "child", "grandchild"} {
			_, err := store.GetSession(ctx, id)
			require.ErrorIs(t, err, ErrNotFound)
			_, err = manifest.LookupGeneratedFile(ctx, id, "image.png")
			require.ErrorIs(t, err, ErrGeneratedFileNotFound)
			_, err = manifest.(GeneratedMediaBlobStore).LookupGeneratedBlob(ctx, id, "image.png")
			require.ErrorIs(t, err, ErrGeneratedBlobNotFound)
		}
		_, err := store.GetSession(ctx, "kept")
		require.NoError(t, err)
		_, err = manifest.LookupGeneratedFile(ctx, "kept", "image.png")
		require.NoError(t, err)
		_, err = manifest.(GeneratedMediaBlobStore).LookupGeneratedBlob(ctx, "kept", "image.png")
		require.NoError(t, err)
		require.ErrorIs(t, store.DeleteSession(ctx, "root"), ErrNotFound)
	})
}

func TestDeleteSessionCanceledBeforeMutation(t *testing.T) {
	t.Parallel()
	manifestStores(t, func(t *testing.T, store Store, _ GeneratedMediaManifest) {
		t.Helper()
		require.NoError(t, store.AddSession(t.Context(), New(WithID("root"))))
		require.NoError(t, store.AddSession(t.Context(), New(WithID("child"), WithParentID("root"))))
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		require.ErrorIs(t, store.DeleteSession(ctx, "root"), context.Canceled)
		for _, id := range []string{"root", "child"} {
			_, err := store.GetSession(t.Context(), id)
			require.NoError(t, err)
		}
	})
}

func TestSQLiteDeleteSubtreeRollback(t *testing.T) {
	t.Parallel()
	store := openMemoryStore(t)
	ctx := t.Context()
	require.NoError(t, store.AddSession(ctx, New(WithID("root"))))
	require.NoError(t, store.AddSession(ctx, New(WithID("child"), WithParentID("root"))))
	require.NoError(t, store.AddGeneratedBlob(ctx, "child", "image.png", []byte("kept")))
	require.NoError(t, store.AddGeneratedFile(ctx, GeneratedFile{SessionID: "child", RelPath: "image.png"}))
	_, err := store.db.ExecContext(ctx, `CREATE TRIGGER block_delete BEFORE DELETE ON sessions BEGIN SELECT RAISE(ABORT, 'delete blocked'); END`)
	require.NoError(t, err)
	require.ErrorContains(t, store.DeleteSession(ctx, "root"), "delete blocked")
	_, err = store.GetSession(ctx, "root")
	require.NoError(t, err)
	_, err = store.GetSession(ctx, "child")
	require.NoError(t, err)
	blob, err := store.LookupGeneratedBlob(ctx, "child", "image.png")
	require.NoError(t, err)
	assert.Equal(t, []byte("kept"), blob)
	_, err = store.LookupGeneratedFile(ctx, "child", "image.png")
	require.NoError(t, err)
}

func TestDeleteSessionPrunesEmbeddedGrandchildMedia(t *testing.T) {
	t.Parallel()
	manifestStores(t, func(t *testing.T, store Store, manifest GeneratedMediaManifest) {
		t.Helper()
		root := New(WithID("root"))
		require.NoError(t, store.AddSession(t.Context(), root))
		child := New(WithID("child"), WithParentID(root.ID))
		grandchild := New(WithID("grandchild"), WithParentID(child.ID))
		child.AddSubSession(grandchild)
		require.NoError(t, store.AddSubSession(t.Context(), root.ID, child))
		require.NoError(t, manifest.AddGeneratedFile(t.Context(), GeneratedFile{SessionID: grandchild.ID, RelPath: "image.png"}))
		blobs := manifest.(GeneratedMediaBlobStore)
		require.NoError(t, blobs.AddGeneratedBlob(t.Context(), grandchild.ID, "image.png", []byte("bytes")))
		require.NoError(t, store.DeleteSession(t.Context(), root.ID))
		_, err := manifest.LookupGeneratedFile(t.Context(), grandchild.ID, "image.png")
		require.ErrorIs(t, err, ErrGeneratedFileNotFound)
		_, err = blobs.LookupGeneratedBlob(t.Context(), grandchild.ID, "image.png")
		require.ErrorIs(t, err, ErrGeneratedBlobNotFound)
	})
}

func TestDeleteSessionConcurrentUnrelatedChildRegistration(t *testing.T) {
	t.Parallel()
	store := NewInMemorySessionStore()
	root := New(WithID("unrelated"))
	require.NoError(t, store.AddSession(t.Context(), root))
	var children []*Session
	for i := range 100 {
		child := New(WithID("child-" + strconv.Itoa(i)))
		root.AddLiveSubSession(child)
		children = append(children, child)
		require.NoError(t, store.AddSession(t.Context(), New(WithID("delete-"+strconv.Itoa(i)))))
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		for _, child := range children {
			assert.NoError(t, store.AddSubSession(t.Context(), root.ID, child))
		}
	})
	wg.Go(func() {
		for i := range 100 {
			assert.NoError(t, store.DeleteSession(t.Context(), "delete-"+strconv.Itoa(i)))
		}
	})
	wg.Wait()
	for _, child := range children {
		_, err := store.GetSession(t.Context(), child.ID)
		require.NoError(t, err)
	}
}
