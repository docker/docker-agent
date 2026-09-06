package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCaptureLocalWorkingDir(t *testing.T) {
	base := t.TempDir()
	// Resolve symlinks (macOS /var → /private/var) so cwd-derived values
	// compare equal to the TempDir path.
	base, err := filepath.EvalSymlinks(base)
	require.NoError(t, err)
	t.Chdir(base)

	t.Run("empty captures process cwd", func(t *testing.T) {
		got, err := CaptureLocalWorkingDir("")
		require.NoError(t, err)
		assert.Equal(t, base, got)
	})

	t.Run("whitespace-only captures process cwd", func(t *testing.T) {
		got, err := CaptureLocalWorkingDir("   ")
		require.NoError(t, err)
		assert.Equal(t, base, got)
	})

	t.Run("relative is absolutized against cwd", func(t *testing.T) {
		require.NoError(t, os.Mkdir(filepath.Join(base, "sub"), 0o755))
		got, err := CaptureLocalWorkingDir("sub")
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(base, "sub"), got)
	})

	t.Run("absolute is cleaned", func(t *testing.T) {
		got, err := CaptureLocalWorkingDir(base + string(filepath.Separator) + "a" + string(filepath.Separator) + ".." + string(filepath.Separator) + "b")
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(base, "b"), got)
	})
}

// mapLookup builds a Lookup over a fixed set of sessions.
func mapLookup(sessions ...*Session) Lookup {
	byID := make(map[string]*Session, len(sessions))
	for _, s := range sessions {
		byID[s.ID] = s
	}
	return func(_ context.Context, id string) (*Session, error) {
		s, ok := byID[id]
		if !ok {
			return nil, ErrNotFound
		}
		return s, nil
	}
}

func TestResolveWorkingDir_OwnRoot(t *testing.T) {
	t.Parallel()
	workingDir := filepath.Join(t.TempDir(), "work", "project")
	sess := &Session{ID: "s1", WorkingDir: workingDir}

	got, err := ResolveWorkingDir(t.Context(), sess, nil)
	require.NoError(t, err)
	assert.Equal(t, workingDir, got)
}

func TestResolveWorkingDir_RootNeedNotExist(t *testing.T) {
	t.Parallel()
	// A deleted workspace is still valid provenance; existence checks belong
	// to whoever opens files under the root.
	workingDir := filepath.Join(t.TempDir(), "definitely", "gone", "workspace")
	sess := &Session{ID: "s1", WorkingDir: workingDir}

	got, err := ResolveWorkingDir(t.Context(), sess, nil)
	require.NoError(t, err)
	assert.Equal(t, workingDir, got)
}

func TestResolveWorkingDir_InheritsFromParentChain(t *testing.T) {
	t.Parallel()
	workingDir := filepath.Join(t.TempDir(), "work", "a")
	root := &Session{ID: "root", WorkingDir: workingDir}
	mid := &Session{ID: "mid", ParentID: "root"}
	leaf := &Session{ID: "leaf", ParentID: "mid"}

	got, err := ResolveWorkingDir(t.Context(), leaf, mapLookup(root, mid))
	require.NoError(t, err)
	assert.Equal(t, workingDir, got)
}

func TestResolveWorkingDir_Failures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		sess   *Session
		lookup Lookup
	}{
		{name: "nil session", sess: nil},
		{name: "empty with no parent", sess: &Session{ID: "s1"}},
		{
			name:   "missing parent",
			sess:   &Session{ID: "s1", ParentID: "gone"},
			lookup: mapLookup(),
		},
		{name: "relative root rejected", sess: &Session{ID: "s1", WorkingDir: "relative/dir"}},
		{name: "dot root rejected", sess: &Session{ID: "s1", WorkingDir: "."}},
		{name: "whitespace-padded root rejected", sess: &Session{ID: "s1", WorkingDir: " /work "}},
		{name: "NUL root rejected", sess: &Session{ID: "s1", WorkingDir: "/work\x00evil"}},
		{
			name: "relative parent root rejected, not repaired",
			sess: &Session{ID: "leaf", ParentID: "root"},
			lookup: mapLookup(
				&Session{ID: "root", WorkingDir: "relative"},
			),
		},
		{
			name: "self parent cycle",
			sess: &Session{ID: "s1", ParentID: "s1"},
			lookup: mapLookup(
				&Session{ID: "s1", ParentID: "s1"},
			),
		},
		{
			name: "two-node cycle",
			sess: &Session{ID: "a", ParentID: "b"},
			lookup: mapLookup(
				&Session{ID: "a", ParentID: "b"},
				&Session{ID: "b", ParentID: "a"},
			),
		},
		{name: "traversal without lookup", sess: &Session{ID: "s1", ParentID: "p1"}},
		{
			name: "lookup ID mismatch",
			sess: &Session{ID: "s1", ParentID: "p1"},
			lookup: func(context.Context, string) (*Session, error) {
				return &Session{ID: "other"}, nil
			},
		},
		{
			name: "nil parent from lookup",
			sess: &Session{ID: "s1", ParentID: "p1"},
			lookup: func(context.Context, string) (*Session, error) {
				return nil, nil
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ResolveWorkingDir(t.Context(), tc.sess, tc.lookup)
			require.ErrorIs(t, err, ErrWorkingDirUnavailable)
			assert.Empty(t, got)
		})
	}
}

// chainOfDepth builds a lookup over a parent chain of n sessions where only
// the topmost ancestor carries a WorkingDir, and returns the leaf.
func chainOfDepth(n int, root string) (*Session, Lookup) {
	sessions := make([]*Session, 0, n)
	for i := range n {
		s := &Session{ID: fmt.Sprintf("s%d", i)}
		if i < n-1 {
			s.ParentID = fmt.Sprintf("s%d", i+1)
		} else {
			s.WorkingDir = root
		}
		sessions = append(sessions, s)
	}
	return sessions[0], mapLookup(sessions...)
}

func TestResolveWorkingDir_DepthBound(t *testing.T) {
	t.Parallel()

	t.Run("exactly at bound succeeds", func(t *testing.T) {
		t.Parallel()
		workingDir := filepath.Join(t.TempDir(), "work", "deep")
		leaf, lookup := chainOfDepth(maxWorkingDirAncestry, workingDir)
		got, err := ResolveWorkingDir(t.Context(), leaf, lookup)
		require.NoError(t, err)
		assert.Equal(t, workingDir, got)
	})

	t.Run("one beyond bound fails", func(t *testing.T) {
		t.Parallel()
		workingDir := filepath.Join(t.TempDir(), "work", "deep")
		leaf, lookup := chainOfDepth(maxWorkingDirAncestry+1, workingDir)
		_, err := ResolveWorkingDir(t.Context(), leaf, lookup)
		require.ErrorIs(t, err, ErrWorkingDirUnavailable)
	})
}

// TestResolveWorkingDir_CrossWorkspaceInvariant pins the media-safety
// invariant: a session created in workspace A and resolved from a process
// running in workspace B yields A (or fails closed) — never B, even when B
// contains a same-named candidate. Not parallel: it changes the process cwd.
func TestResolveWorkingDir_CrossWorkspaceInvariant(t *testing.T) {
	workspaceA := t.TempDir()
	workspaceB := t.TempDir()
	t.Chdir(workspaceB)

	store := NewInMemorySessionStore()
	root := New(WithWorkingDir(workspaceA))
	require.NoError(t, store.AddSession(t.Context(), root))
	// Old-style sub-session persisted before creation-time provenance:
	// empty WorkingDir, linked to its parent.
	child := New(WithParentID(root.ID))
	require.NoError(t, store.AddSubSession(t.Context(), root.ID, child))

	reloadedRoot, err := store.GetSession(t.Context(), root.ID)
	require.NoError(t, err)
	got, err := ResolveWorkingDir(t.Context(), reloadedRoot, store.GetSession)
	require.NoError(t, err)
	assert.Equal(t, workspaceA, got)

	reloadedChild, err := store.GetSession(t.Context(), child.ID)
	require.NoError(t, err)
	got, err = ResolveWorkingDir(t.Context(), reloadedChild, store.GetSession)
	require.NoError(t, err)
	assert.Equal(t, workspaceA, got, "old empty sub-session must inherit its parent's workspace, not the viewer cwd")

	// A workspace-less session fails closed instead of picking up B.
	orphan := New()
	_, err = ResolveWorkingDir(t.Context(), orphan, store.GetSession)
	require.ErrorIs(t, err, ErrWorkingDirUnavailable)

	// A persisted relative root is rejected, never resolved against B.
	relative := &Session{ID: "rel", WorkingDir: "."}
	_, err = ResolveWorkingDir(t.Context(), relative, store.GetSession)
	require.ErrorIs(t, err, ErrWorkingDirUnavailable)
}

// TestSubSessionWorkingDirRoundTrip pins that a sub-session's WorkingDir and
// parent link survive persistence in both store implementations, and that a
// legacy empty sub-session resolves through its stored parent.
func TestSubSessionWorkingDirRoundTrip(t *testing.T) {
	t.Parallel()

	stores := map[string]func(t *testing.T) Store{
		"in-memory": func(*testing.T) Store { return NewInMemorySessionStore() },
		"sqlite": func(t *testing.T) Store {
			t.Helper()
			store, err := newSQLiteStoreForTest(t, filepath.Join(t.TempDir(), "sessions.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			return store
		},
	}

	for name, newStore := range stores {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := newStore(t)
			workingDir := filepath.Join(t.TempDir(), "work", "project")

			parent := New(WithWorkingDir(workingDir))
			require.NoError(t, store.AddSession(t.Context(), parent))

			child := New(WithWorkingDir(parent.WorkingDir))
			require.NoError(t, store.AddSubSession(t.Context(), parent.ID, child))

			reloaded, err := store.GetSession(t.Context(), child.ID)
			require.NoError(t, err)
			assert.Equal(t, parent.ID, reloaded.ParentID)
			assert.Equal(t, workingDir, reloaded.WorkingDir)

			// Legacy sub-session persisted without provenance: stays empty in
			// the store and resolves via the parent chain.
			legacy := New()
			require.NoError(t, store.AddSubSession(t.Context(), parent.ID, legacy))
			reloadedLegacy, err := store.GetSession(t.Context(), legacy.ID)
			require.NoError(t, err)
			assert.Empty(t, reloadedLegacy.WorkingDir)

			resolved, err := ResolveWorkingDir(t.Context(), reloadedLegacy, store.GetSession)
			require.NoError(t, err)
			assert.Equal(t, workingDir, resolved)
		})
	}
}
