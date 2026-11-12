package session

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
)

func TestStoreEvaluationRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "sessions.db")
	store, err := newSQLiteStoreForTest(t, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	sess := New()
	sess.SetUsage(12, 3)
	require.NoError(t, store.AddSession(ctx, sess))
	_, err = store.AddMessage(ctx, sess.ID, UserMessage("before"))
	require.NoError(t, err)

	cost := 0.25
	evaluations := []*Evaluation{
		{ID: "unknown", Evaluator: "judge", AgentName: "root", Model: "judge-model", CreatedAt: "2026-09-22T12:00:00Z"},
		{ID: "free", Evaluator: "judge", Cost: new(float64), Usage: &chat.Usage{}},
		{ID: "paid", Evaluator: "judge", Cost: &cost, Usage: &chat.Usage{InputTokens: 10, CachedInputTokens: 20, CacheWriteTokens: 30, OutputTokens: 40, ReasoningTokens: 5}},
	}
	for _, e := range evaluations {
		sess.AddEvaluation(e)
		require.NoError(t, store.AddEvaluation(ctx, sess.ID, e))
		require.NoError(t, store.AddEvaluation(ctx, sess.ID, e))
		require.NoError(t, store.UpdateSessionTokens(ctx, sess.ID, 12, 3, sess.TotalCost()))
	}
	require.NoError(t, store.AddEvaluation(ctx, sess.ID, &Evaluation{ID: "paid", Evaluator: "must not overwrite"}))
	require.NoError(t, store.Close())

	reloadedStore, err := newSQLiteStoreForTest(t, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reloadedStore.Close() })
	got, err := reloadedStore.GetSession(ctx, sess.ID)
	require.NoError(t, err)
	require.Len(t, got.Messages, 4)
	assert.Equal(t, "before", got.Messages[0].Message.Message.Content)
	for i, e := range evaluations {
		assert.Equal(t, e, got.Messages[i+1].Evaluation)
	}
	assert.InDelta(t, 0.25, got.TotalCost(), 1e-9)
	assertStoredEvaluationCost(t, reloadedStore, sess.ID, 0.25)
	input, output := got.Usage()
	assert.Equal(t, int64(12), input)
	assert.Equal(t, int64(3), output)
}

func TestStoreEvaluationImportNestedItems(t *testing.T) {
	t.Parallel()

	for _, granular := range []bool{false, true} {
		name := "AddSession"
		if granular {
			name = "AddSubSession"
		}
		t.Run(name, func(t *testing.T) {
			store := openMemoryStore(t)
			parent, child, grandchild := New(), New(), New()
			cost := 0.75
			parent.AddEvaluation(&Evaluation{ID: "root", Cost: new(float64)})
			child.AddEvaluation(&Evaluation{ID: "child"})
			grandchild.AddEvaluation(&Evaluation{ID: "grandchild", Cost: &cost, Usage: &chat.Usage{InputTokens: 10}})
			child.AddSubSession(grandchild)
			child.SetTokensAndCost(0, 0, 99)
			grandchild.SetTokensAndCost(0, 0, 99)
			if granular {
				require.NoError(t, store.AddSession(t.Context(), parent))
				require.NoError(t, store.AddSubSession(t.Context(), parent.ID, child))
			} else {
				parent.AddSubSession(child)
				require.NoError(t, store.AddSession(t.Context(), parent))
			}
			got, err := store.GetSession(t.Context(), parent.ID)
			require.NoError(t, err)
			require.Len(t, got.Messages, 2)
			assert.Equal(t, parent.Messages[0].Evaluation, got.Messages[0].Evaluation)
			loadedChild := got.Messages[1].SubSession
			require.NotNil(t, loadedChild)
			require.Len(t, loadedChild.Messages, 2)
			assert.Equal(t, child.Messages[0].Evaluation, loadedChild.Messages[0].Evaluation)
			assert.Equal(t, grandchild.Messages[0].Evaluation, loadedChild.Messages[1].SubSession.Messages[0].Evaluation)
			assert.InDelta(t, 0.75, loadedChild.Cost, 1e-9)
			assert.InDelta(t, 0.75, loadedChild.Messages[1].SubSession.Cost, 1e-9)
			assert.InDelta(t, 0.75, got.TotalCost(), 1e-9)
		})
	}
}

func TestInMemoryStoreEvaluationLiveAlias(t *testing.T) {
	t.Parallel()

	for _, runtimeFirst := range []bool{false, true} {
		name := "store first"
		if runtimeFirst {
			name = "runtime first"
		}
		t.Run(name, func(t *testing.T) {
			store := NewInMemorySessionStore()
			sess := New()
			require.NoError(t, store.AddSession(t.Context(), sess))
			cost := 0.25
			e := &Evaluation{ID: "request", Cost: &cost, Usage: &chat.Usage{InputTokens: 10}}
			if runtimeFirst {
				sess.AddEvaluation(e)
			}
			require.NoError(t, store.AddEvaluation(t.Context(), sess.ID, e))
			sess.AddEvaluation(e)
			e.Usage.InputTokens = 999
			cost = 999

			got, err := store.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			require.Same(t, sess, got)
			require.Len(t, got.MessagesSnapshot(), 1)
			assert.Equal(t, int64(10), got.MessagesSnapshot()[0].Evaluation.Usage.InputTokens)
			assert.InDelta(t, 0.25, got.TotalCost(), 1e-9)
		})
	}
}

func TestStoreEvaluationConcurrentDeduplication(t *testing.T) {
	t.Parallel()

	for name, store := range map[string]Store{"memory": NewInMemorySessionStore(), "sqlite": openMemoryStore(t)} {
		t.Run(name, func(t *testing.T) {
			sess := New()
			require.NoError(t, store.AddSession(t.Context(), sess))
			cost := 0.25
			e := &Evaluation{ID: "request", Cost: &cost, Usage: &chat.Usage{InputTokens: 10}}
			var wg sync.WaitGroup
			for range 20 {
				wg.Go(func() { assert.NoError(t, store.AddEvaluation(t.Context(), sess.ID, e)) })
			}
			wg.Wait()
			got, err := store.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			assert.Equal(t, 1, got.ItemCount())
			assert.InDelta(t, 0.25, got.TotalCost(), 1e-9)
			require.ErrorIs(t, store.AddEvaluation(t.Context(), "", e), ErrEmptyID)
		})
	}
}
