package session

import (
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
)

func TestAddEvaluationSynchronizesCost(t *testing.T) {
	t.Parallel()

	sess, child := New(), New()
	sess.AddMessage(&Message{Message: chat.Message{Cost: 0.5}})
	sess.Messages = append(sess.Messages, Item{Summary: "summary", Cost: 0.125})
	child.AddMessage(&Message{Message: chat.Message{Cost: 0.25}})
	sess.AddSubSession(child)
	sess.SetTokensAndCost(12, 3, 99)
	sess.AddEvaluation(nil)
	_, _, cost := sess.TokensAndCost()
	assert.InDelta(t, 99.0, cost, 1e-9, "nil must remain a no-op")

	paid := 0.125
	e := &Evaluation{ID: "paid", Cost: &paid, Usage: &chat.Usage{InputTokens: 1000}}
	sess.AddEvaluation(e)
	sess.AddEvaluation(e)
	sess.AddEvaluation(&Evaluation{ID: "unknown"})
	sess.AddEvaluation(&Evaluation{ID: "free", Cost: new(float64)})
	input, output, cost := sess.TokensAndCost()
	assert.Equal(t, int64(12), input)
	assert.Equal(t, int64(3), output)
	assert.InDelta(t, 1.0, cost, 1e-9)
	assert.InDelta(t, sess.TotalCost(), cost, 1e-9)

	payload, err := json.Marshal(sess)
	require.NoError(t, err)
	var decoded Session
	require.NoError(t, json.Unmarshal(payload, &decoded))
	assert.InDelta(t, 1.0, decoded.Cost, 1e-9)
}

func TestStoreEvaluationCostImport(t *testing.T) {
	t.Parallel()

	for _, backend := range []string{"memory", "sqlite"} {
		for _, tc := range []struct {
			name string
			cost *float64
		}{
			{name: "unknown"},
			{name: "free", cost: new(float64)},
			{name: "paid", cost: new(0.25)},
		} {
			t.Run(backend+"/"+tc.name, func(t *testing.T) {
				store := evaluationCostStore(t, backend)
				sess := New()
				sess.SetTokensAndCost(12, 3, 99)
				// Imported JSON can contain items without passing through AddEvaluation.
				sess.Messages = []Item{
					{Message: &Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Cost: 0.5}}},
					{Summary: "summary", Cost: 0.125},
					{Evaluation: &Evaluation{ID: "request", Cost: tc.cost}},
				}
				want := sess.TotalCost()
				require.NoError(t, store.AddSession(t.Context(), sess))
				assertStoredEvaluationCost(t, store, sess.ID, want)
				got, err := store.GetSession(t.Context(), sess.ID)
				require.NoError(t, err)
				input, output := got.Usage()
				assert.Equal(t, int64(12), input)
				assert.Equal(t, int64(3), output)
				assert.Equal(t, tc.cost, got.MessagesSnapshot()[2].Evaluation.Cost)
				// Repeated metadata writes replace the total, rather than adding spend.
				for range 2 {
					require.NoError(t, store.UpdateSession(t.Context(), sess))
					assertStoredEvaluationCost(t, store, sess.ID, want)
				}
			})
		}
	}
}

func TestStoreEvaluationCostUpdateAfterTokens(t *testing.T) {
	t.Parallel()

	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			store := evaluationCostStore(t, backend)
			sess := New()
			// UpdateSession creates a separate metadata-only stored session.
			require.NoError(t, store.UpdateSession(t.Context(), sess))
			msg := &Message{Message: chat.Message{Role: chat.MessageRoleAssistant, Cost: 0.5}}
			sess.AddMessage(msg)
			_, err := store.AddMessage(t.Context(), sess.ID, msg)
			require.NoError(t, err)
			e := &Evaluation{ID: "request", Cost: new(0.25)}
			sess.AddEvaluation(e)
			// Match the observer: persist the item, then the cumulative token event.
			require.NoError(t, store.AddEvaluation(t.Context(), sess.ID, e))
			require.NoError(t, store.UpdateSessionTokens(t.Context(), sess.ID, 12, 3, 0.75))
			assertStoredEvaluationCost(t, store, sess.ID, 0.75)

			// A later metadata snapshot must not restore an older scalar cost.
			sess.SetTokensAndCost(12, 3, 0.5)
			for range 2 {
				require.NoError(t, store.UpdateSession(t.Context(), sess))
				require.NoError(t, store.AddEvaluation(t.Context(), sess.ID, e))
				assertStoredEvaluationCost(t, store, sess.ID, 0.75)
			}
			got, err := store.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			assert.Equal(t, 2, got.ItemCount())
			input, output := got.Usage()
			assert.Equal(t, int64(12), input)
			assert.Equal(t, int64(3), output)
		})
	}
}

func TestStoreEvaluationCostNestedImport(t *testing.T) {
	t.Parallel()

	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			store := evaluationCostStore(t, backend)
			parent, child, grandchild := New(), New(), New()
			parent.AddMessage(&Message{Message: chat.Message{Cost: 0.5}})
			child.Messages = []Item{{Summary: "summary", Cost: 0.125}}
			grandchild.Messages = []Item{{Evaluation: &Evaluation{ID: "nested", Cost: new(0.25)}}}
			child.AddSubSession(grandchild)
			parent.AddSubSession(child)
			for _, sess := range []*Session{parent, child, grandchild} {
				sess.SetTokensAndCost(12, 3, 99)
			}
			require.NoError(t, store.AddSession(t.Context(), parent))
			assertStoredEvaluationCost(t, store, parent.ID, 0.875)
			got, err := store.GetSession(t.Context(), parent.ID)
			require.NoError(t, err)
			loadedChild := got.MessagesSnapshot()[1].SubSession
			require.NotNil(t, loadedChild)
			assert.InDelta(t, 0.375, loadedChild.Cost, 1e-9)
			assert.InDelta(t, 0.25, loadedChild.MessagesSnapshot()[1].SubSession.Cost, 1e-9)

			// The parent has no direct evaluations, but still derives its full total.
			parent.SetTokensAndCost(12, 3, 0.5)
			require.NoError(t, store.UpdateSession(t.Context(), parent))
			assertStoredEvaluationCost(t, store, parent.ID, 0.875)
		})
	}
}

func TestStoreCostWithoutEvaluationsPreservesMetadata(t *testing.T) {
	t.Parallel()

	for _, backend := range []string{"memory", "sqlite"} {
		for _, withItems := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/items=%t", backend, withItems), func(t *testing.T) {
				store := evaluationCostStore(t, backend)
				sess := New()
				if withItems {
					child := New()
					child.AddMessage(&Message{Message: chat.Message{Cost: 0.25}})
					sess.AddSubSession(child)
				}
				sess.AddEvaluationUsageRecord(&Evaluation{ID: "transient", Cost: new(0.5)})
				sess.SetTokensAndCost(12, 3, 17)
				require.NoError(t, store.AddSession(t.Context(), sess))
				assertStoredEvaluationCost(t, store, sess.ID, 17)
				sess.SetTokensAndCost(12, 3, 42)
				require.NoError(t, store.UpdateSession(t.Context(), sess))
				assertStoredEvaluationCost(t, store, sess.ID, 42)
			})
		}
	}
}

func TestStoreEvaluationCostConcurrentSnapshots(t *testing.T) {
	t.Parallel()

	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			store := evaluationCostStore(t, backend)
			parent, child := New(), New()
			parent.AddSubSession(child)
			require.NoError(t, store.AddSession(t.Context(), parent))
			var wg sync.WaitGroup
			wg.Go(func() {
				for i := range 50 {
					child.AddEvaluation(&Evaluation{ID: strconv.Itoa(i), Cost: new(0.25)})
					parent.SetTokensAndCost(int64(i), 3, 0)
				}
			})
			wg.Go(func() {
				for range 50 {
					if err := store.UpdateSession(t.Context(), parent); err != nil {
						t.Errorf("updating session: %v", err)
					}
					_, _, _ = child.TokensAndCost()
				}
			})
			wg.Wait()
			require.NoError(t, store.UpdateSession(t.Context(), parent))
			assertStoredEvaluationCost(t, store, parent.ID, 12.5)
		})
	}
}

func evaluationCostStore(t *testing.T, backend string) Store {
	t.Helper()
	if backend == "memory" {
		return NewInMemorySessionStore()
	}
	return openMemoryStore(t)
}

func assertStoredEvaluationCost(t *testing.T, store Store, id string, want float64) {
	t.Helper()
	got, err := store.GetSession(t.Context(), id)
	require.NoError(t, err)
	_, _, cost := got.TokensAndCost()
	assert.InDelta(t, want, cost, 1e-9)
	summaries, err := store.GetSessionSummaries(t.Context())
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	assert.Equal(t, id, summaries[0].ID)
	assert.InDelta(t, want, summaries[0].Cost, 1e-9)
}
