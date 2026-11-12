package session

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
)

func testRoutingDecision(id string) *RoutingDecision {
	probability := 0.91
	return &RoutingDecision{
		ID: id, InvocationID: "invocation", StepID: "step-1", Phase: "before_agent_run",
		FromAgent: "root", ToAgent: "specialist", Action: "route",
		Evaluator: "task_route", Selected: "complex", Probability: &probability, Model: "judge",
		CreatedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
	}
}

func TestAddRoutingDecisionDeduplicatesByID(t *testing.T) {
	t.Parallel()

	sess := New()
	decision := testRoutingDecision("one")
	sess.AddRoutingDecision(decision)
	sess.AddRoutingDecision(decision)
	sess.AddRoutingDecision(nil)

	history := sess.RoutingDecisionHistory()
	require.Len(t, history, 1)
	assert.Equal(t, decision, history[0])

	// The stored record must not alias the caller's value.
	*decision.Probability = 0.1
	assert.InDelta(t, 0.91, *sess.RoutingDecisionHistory()[0].Probability, 1e-9)
}

func TestRoutingDecisionPersistsInTranscriptOrder(t *testing.T) {
	t.Parallel()

	store, err := newSQLiteStoreForTest(t, filepath.Join(t.TempDir(), "routing.db"))
	require.NoError(t, err)
	defer store.(*SQLiteSessionStore).Close()

	sess := &Session{ID: "routed", CreatedAt: time.Now(), Messages: []Item{NewMessageItem(UserMessage("hello"))}}
	require.NoError(t, store.AddSession(t.Context(), sess))
	require.NoError(t, store.AddRoutingDecision(t.Context(), sess.ID, testRoutingDecision("one")))
	require.NoError(t, store.AddRoutingDecision(t.Context(), sess.ID, testRoutingDecision("one")), "replays are idempotent")
	require.NoError(t, store.AddRoutingDecision(t.Context(), sess.ID, testRoutingDecision("two")))
	require.NoError(t, store.AddRoutingDecision(t.Context(), sess.ID, nil))

	loaded, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, loaded.Messages, 3)
	assert.NotNil(t, loaded.Messages[0].Message)
	require.NotNil(t, loaded.Messages[1].RoutingDecision)
	assert.Equal(t, "one", loaded.Messages[1].RoutingDecision.ID)
	assert.Equal(t, "two", loaded.Messages[2].RoutingDecision.ID)
	assert.Equal(t, testRoutingDecision("one"), loaded.Messages[1].RoutingDecision)
}

func TestRoutingDecisionDoesNotEnterConversationOrCost(t *testing.T) {
	t.Parallel()

	sess := New()
	sess.AddMessage(UserMessage("hello"))
	a := agent.New("root", "instructions")
	before := sess.GetMessages(a)
	sess.AddRoutingDecision(testRoutingDecision("one"))

	assert.Equal(t, before, sess.GetMessages(a), "decisions are audit records, not model context")
	assert.Zero(t, sess.TotalCost())
}

func TestRoutingDecisionSurvivesBranchAndClone(t *testing.T) {
	t.Parallel()

	parent := &Session{ID: "parent", CreatedAt: time.Now(), Messages: []Item{
		NewMessageItem(UserMessage("first")),
		{RoutingDecision: testRoutingDecision("one")},
		NewMessageItem(UserMessage("second")),
	}}

	branched, err := BranchSession(parent, 2)
	require.NoError(t, err)
	history := branched.RoutingDecisionHistory()
	require.Len(t, history, 1)
	assert.Equal(t, "one", history[0].ID)
	assert.NotSame(t, parent.Messages[1].RoutingDecision, branched.Messages[1].RoutingDecision)

	cloned := parent.Clone()
	require.Len(t, cloned.RoutingDecisionHistory(), 1)
	assert.NotSame(t, parent.Messages[1].RoutingDecision, cloned.Messages[1].RoutingDecision)
}

func TestInMemoryStoreAddsRoutingDecision(t *testing.T) {
	t.Parallel()

	store := NewInMemorySessionStore()
	sess := New()
	require.NoError(t, store.AddSession(t.Context(), sess))
	require.NoError(t, store.AddRoutingDecision(t.Context(), sess.ID, testRoutingDecision("one")))
	require.ErrorIs(t, store.AddRoutingDecision(t.Context(), "", testRoutingDecision("one")), ErrEmptyID)
	require.ErrorIs(t, store.AddRoutingDecision(t.Context(), "missing", testRoutingDecision("one")), ErrNotFound)

	loaded, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	assert.Len(t, loaded.RoutingDecisionHistory(), 1)
}
