package session

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
)

func TestSQLiteAddSubSessionExistingRow(t *testing.T) {
	t.Parallel()

	for _, withItems := range []bool{false, true} {
		name := "metadata only"
		if withItems {
			name = "partial transcript"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "sessions.db")
			store, err := newSQLiteStoreForTest(t, path)
			require.NoError(t, err)
			t.Cleanup(func() { _ = store.Close() })
			parent := New(WithID("parent"), WithUserMessage("start"))
			require.NoError(t, store.AddSession(ctx, parent))
			child := New(WithID("child"), WithParentID(parent.ID), WithUserMessage("task"))
			if withItems {
				require.NoError(t, store.AddSession(ctx, child))
				child.Messages[0].Message.ID = 123
			} else {
				require.NoError(t, store.UpdateSession(ctx, child))
			}

			child.AddMessage(NewAgentMessage("worker", &chat.Message{
				Role:    chat.MessageRoleAssistant,
				Content: "finished",
				Model:   "test/model",
				Usage:   &chat.Usage{InputTokens: 100, OutputTokens: 20},
				Cost:    0.25,
			}))
			child.SetTokensAndCost(100, 20, 0.25)
			child.SetAttribute("completed", "true")
			require.NoError(t, store.AddSubSession(ctx, parent.ID, child))
			_, err = store.AddMessage(ctx, parent.ID, UserMessage("after completion"))
			require.NoError(t, err)
			require.NoError(t, store.AddSubSession(ctx, parent.ID, child), "repeated completion must not duplicate items")
			require.NoError(t, store.Close())

			reopened, err := newSQLiteStoreForTest(t, path)
			require.NoError(t, err)
			t.Cleanup(func() { _ = reopened.Close() })
			loaded, err := reopened.GetSession(ctx, parent.ID)
			require.NoError(t, err)
			require.Len(t, loaded.Messages, 3)
			var itemCount int
			require.NoError(t, reopened.(*SQLiteSessionStore).db.QueryRowContext(ctx, "SELECT COUNT(*) FROM session_items WHERE session_id = ?", parent.ID).Scan(&itemCount))
			assert.Equal(t, 3, itemCount, "repeated completion must not leave orphaned parent references")
			assert.Equal(t, "start", loaded.Messages[0].Message.Message.Content)
			assert.Equal(t, "after completion", loaded.Messages[2].Message.Message.Content)
			got := loaded.Messages[1].SubSession
			require.NotNil(t, got)
			assert.Equal(t, parent.ID, got.ParentID)
			require.Len(t, got.Messages, 2)
			assert.Equal(t, "task", got.Messages[0].Message.Message.Content)
			assert.Equal(t, child.Messages[1].Message.Message, got.Messages[1].Message.Message)
			assert.Equal(t, child.AttributesSnapshot(), got.AttributesSnapshot())
			input, output, cost := got.TokensAndCost()
			assert.Equal(t, int64(100), input)
			assert.Equal(t, int64(20), output)
			assert.InDelta(t, 0.25, cost, 1e-9)
			assert.InDelta(t, 0.25, loaded.TotalCost(), 1e-9)
			summaries, err := reopened.GetSessionSummaries(ctx)
			require.NoError(t, err)
			require.Len(t, summaries, 1)
			assert.Equal(t, parent.ID, summaries[0].ID)
		})
	}
}

func TestSQLiteAddSubSessionNestedExistingRows(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, err := newSQLiteStoreForTest(t, filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	parent := New(WithID("parent"))
	require.NoError(t, store.AddSession(ctx, parent))
	child := New(WithID("child"), WithParentID(parent.ID), WithUserMessage("child task"))
	require.NoError(t, store.UpdateSession(ctx, child))
	grandchild := New(WithID("grandchild"), WithParentID(child.ID), WithUserMessage("nested task"))
	require.NoError(t, store.UpdateSession(ctx, grandchild))
	grandchild.AddMessage(NewAgentMessage("worker", &chat.Message{
		Role: chat.MessageRoleAssistant, Content: "nested result", Cost: 0.25,
	}))
	grandchild.SetTokensAndCost(100, 20, 0.25)
	child.AddSubSession(grandchild)

	require.NoError(t, store.AddSubSession(ctx, parent.ID, child))
	require.NoError(t, store.AddSubSession(ctx, parent.ID, child))
	loaded, err := store.GetSession(ctx, parent.ID)
	require.NoError(t, err)
	require.Len(t, loaded.Messages, 1)
	gotChild := loaded.Messages[0].SubSession
	require.NotNil(t, gotChild)
	require.Len(t, gotChild.Messages, 2)
	got := gotChild.Messages[1].SubSession
	require.NotNil(t, got)
	assert.Equal(t, child.ID, got.ParentID)
	assert.Equal(t, "nested result", got.GetLastAssistantMessageContent())
	assert.InDelta(t, 0.25, loaded.TotalCost(), 1e-9)
	assert.Equal(t, int64(100), got.InputTokens)
	assert.Equal(t, int64(20), got.OutputTokens)
}

func TestSQLiteAddSubSessionRollback(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"child items", "parent reference"} {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			store, err := newSQLiteStoreForTest(t, filepath.Join(t.TempDir(), "sessions.db"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = store.Close() })
			sqlite := store.(*SQLiteSessionStore)
			parent := New(WithID("parent"), WithUserMessage("start"))
			require.NoError(t, store.AddSession(ctx, parent))
			child := New(WithID("child"), WithParentID(parent.ID), WithTitle("original"), WithUserMessage("task"))
			require.NoError(t, store.AddSession(ctx, child))
			before, err := store.GetSession(ctx, child.ID)
			require.NoError(t, err)
			grandchild := New(WithID("grandchild"), WithParentID(child.ID), WithUserMessage("nested task"))
			require.NoError(t, store.UpdateSession(ctx, grandchild))
			beforeGrandchild, err := store.GetSession(ctx, grandchild.ID)
			require.NoError(t, err)
			grandchild.AddMessage(NewAgentMessage("worker", &chat.Message{Role: chat.MessageRoleAssistant, Content: "nested result", Cost: 0.10}))
			grandchild.SetTokensAndCost(50, 10, 0.10)
			child.SetTitle("completed")
			child.AddMessage(NewAgentMessage("worker", &chat.Message{Role: chat.MessageRoleAssistant, Content: "finished", Cost: 0.25}))
			child.SetTokensAndCost(100, 20, 0.25)
			child.AddSubSession(grandchild)
			condition := "NEW.session_id = 'child' AND NEW.position = 1"
			if stage == "parent reference" {
				condition = "NEW.session_id = 'parent' AND NEW.item_type = 'subsession'"
			}
			_, err = sqlite.db.ExecContext(ctx, `CREATE TRIGGER block_subsession BEFORE INSERT ON session_items WHEN `+condition+` BEGIN SELECT RAISE(ABORT, 'persist blocked'); END`)
			require.NoError(t, err)

			require.ErrorContains(t, store.AddSubSession(ctx, parent.ID, child), "persist blocked")
			after, err := store.GetSession(ctx, child.ID)
			require.NoError(t, err)
			assert.Equal(t, before, after, "metadata and the original transcript must both roll back")
			afterGrandchild, err := store.GetSession(ctx, grandchild.ID)
			require.NoError(t, err)
			assert.Equal(t, beforeGrandchild, afterGrandchild, "nested updates must roll back too")
			loaded, err := store.GetSession(ctx, parent.ID)
			require.NoError(t, err)
			require.Len(t, loaded.Messages, 1)
			assert.Equal(t, "start", loaded.Messages[0].Message.Message.Content)
		})
	}
}

func TestSQLiteAddSubSessionRejectsConflictingRow(t *testing.T) {
	t.Parallel()
	for _, conflict := range []string{"origin", "parent", "root"} {
		t.Run(conflict, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			store, err := newSQLiteStoreForTest(t, filepath.Join(t.TempDir(), "sessions.db"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = store.Close() })
			parent := New(WithID("parent"))
			require.NoError(t, store.AddSession(ctx, parent))
			stored := New(WithID("child"), WithParentID(parent.ID), WithTitle("original"), WithUserMessage("kept"))
			switch conflict {
			case "origin":
				stored.Origin = "a2a"
			case "parent":
				require.NoError(t, store.AddSession(ctx, New(WithID("other-parent"))))
				stored.ParentID = "other-parent"
			case "root":
				stored.ParentID = ""
			}
			require.NoError(t, store.AddSession(ctx, stored))
			before, err := store.GetSession(ctx, stored.ID)
			require.NoError(t, err)
			child := New(WithID(stored.ID), WithParentID(parent.ID), WithUserMessage("replacement"))
			err = store.AddSubSession(ctx, parent.ID, child)
			if conflict == "origin" {
				require.ErrorIs(t, err, ErrOriginMismatch)
			} else {
				require.ErrorIs(t, err, ErrAlreadyExists)
			}
			after, err := store.GetSession(ctx, stored.ID)
			require.NoError(t, err)
			assert.Equal(t, before, after)
			loaded, err := store.GetSession(ctx, parent.ID)
			require.NoError(t, err)
			assert.Empty(t, loaded.Messages)
		})
	}
}

func TestSQLiteAddSubSessionNewRowRollback(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, err := newSQLiteStoreForTest(t, filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	parent := New(WithID("parent"))
	require.NoError(t, store.AddSession(ctx, parent))
	child := New(WithID("child"), WithUserMessage("task"))
	_, err = store.(*SQLiteSessionStore).db.ExecContext(ctx, `CREATE TRIGGER block_link BEFORE INSERT ON session_items WHEN NEW.item_type = 'subsession' BEGIN SELECT RAISE(ABORT, 'link blocked'); END`)
	require.NoError(t, err)
	require.ErrorContains(t, store.AddSubSession(ctx, parent.ID, child), "link blocked")
	_, err = store.GetSession(ctx, child.ID)
	require.ErrorIs(t, err, ErrNotFound)
	assert.Empty(t, child.ParentID, "a failed transaction must not mutate the caller's session")
}

func TestSQLiteAddSubSessionAfterCompaction(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, err := newSQLiteStoreForTest(t, filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	parent := New(WithID("parent"))
	require.NoError(t, store.AddSession(ctx, parent))
	child := New(WithID("child"), WithParentID(parent.ID), WithUserMessage("task"))
	child.AddMessage(NewAgentMessage("worker", &chat.Message{Role: chat.MessageRoleAssistant, Content: "before compaction", Cost: 0.25}))
	summary := Item{
		Summary: "summary", FirstKeptEntry: 1, Cost: 0.05, Model: "test/model",
		Usage: &chat.Usage{InputTokens: 100, OutputTokens: 20},
	}
	require.NoError(t, store.PersistCompaction(ctx, child, 40, 0, summary))
	child.AddMessage(NewAgentMessage("worker", &chat.Message{Role: chat.MessageRoleAssistant, Content: "finished", Cost: 0.10}))
	child.SetTokensAndCost(70, 5, child.TotalCost())

	require.NoError(t, store.AddSubSession(ctx, parent.ID, child))
	loaded, err := store.GetSession(ctx, child.ID)
	require.NoError(t, err)
	require.Len(t, loaded.Messages, 4)
	assert.Equal(t, summary, loaded.Messages[2])
	assert.Equal(t, "finished", loaded.GetLastAssistantMessageContent())
	assert.InDelta(t, 0.40, loaded.TotalCost(), 1e-9)
	assert.InDelta(t, 0.40, loaded.Cost, 1e-9)
	assert.Equal(t, int64(70), loaded.InputTokens)
	assert.Equal(t, int64(5), loaded.OutputTokens)
}

func TestSQLiteAddSubSessionConcurrentCompletion(t *testing.T) {
	t.Parallel()
	store, err := newSQLiteStoreForTest(t, filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	parent := New(WithID("parent"))
	require.NoError(t, store.AddSession(t.Context(), parent))
	child := New(WithID("child"), WithParentID(parent.ID), WithUserMessage("task"))
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() { assert.NoError(t, store.AddSubSession(t.Context(), parent.ID, child)) })
	}
	wg.Wait()
	loaded, err := store.GetSession(t.Context(), parent.ID)
	require.NoError(t, err)
	require.Len(t, loaded.Messages, 1)
	require.NotNil(t, loaded.Messages[0].SubSession)
	assert.Len(t, loaded.Messages[0].SubSession.Messages, 1)
}

func TestSQLiteAddSubSessionRejectsIncompleteSnapshot(t *testing.T) {
	t.Parallel()
	for _, nested := range []bool{false, true} {
		name := "message"
		if nested {
			name = "background child"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			store, err := newSQLiteStoreForTest(t, filepath.Join(t.TempDir(), "sessions.db"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = store.Close() })
			parent := New(WithID("parent"))
			require.NoError(t, store.AddSession(ctx, parent))
			child := New(WithID("child"), WithParentID(parent.ID), WithUserMessage("task"))
			require.NoError(t, store.AddSession(ctx, child))
			stale := child.Clone()
			if nested {
				grandchild := New(WithID("grandchild"), WithParentID(child.ID), WithUserMessage("nested task"))
				require.NoError(t, store.AddSubSession(ctx, child.ID, grandchild))
			} else {
				_, err = store.AddMessage(ctx, child.ID, UserMessage("newer history"))
				require.NoError(t, err)
			}
			before, err := store.GetSession(ctx, child.ID)
			require.NoError(t, err)
			require.Error(t, store.AddSubSession(ctx, parent.ID, stale), "an older snapshot must not erase newer stored history")
			after, err := store.GetSession(ctx, child.ID)
			require.NoError(t, err)
			assert.Equal(t, before, after)
		})
	}
}
