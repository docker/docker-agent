package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSQLiteSessionAttributesAddRoundTrip(t *testing.T) {
	t.Parallel()
	store := openMemoryStore(t)
	sess := New(
		WithID("attributes-add"),
		WithAttributes(map[string]string{
			"daw.workspace_path": "/workspace",
			"daw.worktree_id":    "wt-1",
		}),
	)

	require.NoError(t, store.AddSession(t.Context(), sess))
	loaded, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	assert.Equal(t, sess.AttributesSnapshot(), loaded.AttributesSnapshot())
}

func TestSQLiteSessionAttributesUpdateRoundTrip(t *testing.T) {
	t.Parallel()
	store := openMemoryStore(t)
	sess := New(WithID("attributes-update"), WithAttributes(map[string]string{"daw.worktree_id": "wt-1"}))
	require.NoError(t, store.AddSession(t.Context(), sess))

	sess.SetAttribute("daw.worktree_id", "wt-2")
	sess.SetAttribute("daw.worktree_branch", "feature")
	require.NoError(t, store.UpdateSession(t.Context(), sess))

	loaded, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"daw.worktree_id":     "wt-2",
		"daw.worktree_branch": "feature",
	}, loaded.AttributesSnapshot())
}

func TestSQLiteUnrelatedMetadataUpdatePreservesAttributes(t *testing.T) {
	t.Parallel()
	store := openMemoryStore(t)
	sess := New(WithID("attributes-preserved"), WithAttributes(map[string]string{"daw.execution_type": "worktree"}))
	require.NoError(t, store.AddSession(t.Context(), sess))

	loaded, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	loaded.SetTitle("renamed")
	require.NoError(t, store.UpdateSession(t.Context(), loaded))
	require.NoError(t, store.UpdateSessionTitle(t.Context(), sess.ID, "renamed again"))

	reloaded, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"daw.execution_type": "worktree"}, reloaded.AttributesSnapshot())
}

func TestSQLiteSessionSummariesIncludeAttributesWithoutLoadingMessages(t *testing.T) {
	t.Parallel()
	store := openMemoryStore(t)
	sess := New(
		WithID("attributes-summary"),
		WithAttributes(map[string]string{"daw.workspace_path": "/workspace"}),
		WithUserMessage("hello"),
	)
	require.NoError(t, store.AddSession(t.Context(), sess))

	// A full load would fail on this malformed payload. Summaries must obtain
	// attributes directly from the sessions row without loading item history.
	_, err := store.db.ExecContext(t.Context(),
		"UPDATE session_items SET message_json = 'not-json' WHERE session_id = ?", sess.ID)
	require.NoError(t, err)

	summaries, err := store.GetSessionSummaries(t.Context())
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	assert.Equal(t, map[string]string{"daw.workspace_path": "/workspace"}, summaries[0].Attributes)

	summaries[0].Attributes["daw.workspace_path"] = "/mutated"
	assert.Equal(t, "/workspace", sess.AttributesSnapshot()["daw.workspace_path"])
}

func TestInMemorySessionSummaryAttributesAreIndependent(t *testing.T) {
	t.Parallel()
	store := NewInMemorySessionStore()
	sess := New(WithID("memory-attributes"), WithAttributes(map[string]string{"daw.worktree_id": "wt-1"}))
	require.NoError(t, store.AddSession(t.Context(), sess))

	summaries, err := store.GetSessionSummaries(t.Context())
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	summaries[0].Attributes["daw.worktree_id"] = "mutated"
	summaries[0].Attributes["daw.worktree_path"] = "/other"

	assert.Equal(t, map[string]string{"daw.worktree_id": "wt-1"}, sess.AttributesSnapshot())
}

func TestInMemorySessionAttributesUpdateRoundTrip(t *testing.T) {
	t.Parallel()
	store := NewInMemorySessionStore()
	sess := New(WithID("memory-update"), WithAttributes(map[string]string{"daw.worktree_id": "wt-1"}))
	require.NoError(t, store.UpdateSession(t.Context(), sess))

	sess.SetAttribute("daw.worktree_id", "mutated-after-update")
	loaded, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"daw.worktree_id": "wt-1"}, loaded.AttributesSnapshot())
}

func TestSQLiteSessionAttributesLegacyNullAndEmptyValues(t *testing.T) {
	t.Parallel()
	store := openMemoryStore(t)
	for _, id := range []string{"attributes-null", "attributes-empty"} {
		require.NoError(t, store.AddSession(t.Context(), New(WithID(id))))
	}
	_, err := store.db.ExecContext(t.Context(), "UPDATE sessions SET attributes = NULL WHERE id = 'attributes-null'")
	require.NoError(t, err)
	_, err = store.db.ExecContext(t.Context(), "UPDATE sessions SET attributes = '' WHERE id = 'attributes-empty'")
	require.NoError(t, err)

	for _, id := range []string{"attributes-null", "attributes-empty"} {
		loaded, err := store.GetSession(t.Context(), id)
		require.NoError(t, err)
		assert.Empty(t, loaded.AttributesSnapshot())
	}

	summaries, err := store.GetSessionSummaries(t.Context())
	require.NoError(t, err)
	require.Len(t, summaries, 2)
	for _, summary := range summaries {
		assert.Empty(t, summary.Attributes)
	}
}
