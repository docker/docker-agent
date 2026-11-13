package runtime

import (
	"context"
	"database/sql"
	"log/slog"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

// streamingBenchChunks is the number of AgentChoice deltas exercised per
// benchmark iteration — roughly the order of magnitude of a long assistant turn.
const streamingBenchChunks = 500

// countingPersistenceStore wraps a session store and records AddMessage /
// UpdateMessage calls so tests can pin the per-chunk persistence contract.
type countingPersistenceStore struct {
	session.Store

	addCalls    atomic.Int64
	updateCalls atomic.Int64
}

func newCountingPersistenceStore() *countingPersistenceStore {
	return &countingPersistenceStore{
		Store: session.NewInMemorySessionStore(),
	}
}

func (s *countingPersistenceStore) AddMessage(ctx context.Context, sessionID string, msg *session.Message) (int64, error) {
	s.addCalls.Add(1)
	return s.Store.AddMessage(ctx, sessionID, msg)
}

func (s *countingPersistenceStore) UpdateMessage(ctx context.Context, sessionID string, messageID int64, msg *session.Message) error {
	s.updateCalls.Add(1)
	return s.Store.UpdateMessage(ctx, sessionID, messageID, msg)
}

func setupPersistenceObserverBench(tb testing.TB) (*PersistenceObserver, session.Store) {
	tb.Helper()
	store := session.NewInMemorySessionStore()
	obs := newPersistenceObserver(store)
	require.NotNil(tb, obs)
	return obs, store
}

func emitStreamingChunks(ctx context.Context, obs *PersistenceObserver, sess *session.Session, chunks int) {
	for range chunks {
		obs.OnEvent(ctx, sess, AgentChoice("root", sess.ID, "tok"))
	}
}

func finalizeStreamingMessage(ctx context.Context, obs *PersistenceObserver, sess *session.Session) {
	obs.OnEvent(ctx, sess, MessageAdded(sess.ID, &session.Message{
		AgentName: "root",
		Message: chat.Message{
			Role:    chat.MessageRoleAssistant,
			Content: "done",
		},
	}, "root"))
}

// TestPersistenceObserver_UpdateCountPerChunk documents that each streaming
// delta triggers a store write: one AddMessage on the first chunk, then one
// UpdateMessage per subsequent chunk until MessageAddedEvent finalises the row.
func TestPersistenceObserver_UpdateCountPerChunk(t *testing.T) {
	t.Parallel()

	const chunks = 100
	ctx := t.Context()

	store := newCountingPersistenceStore()
	obs := newPersistenceObserver(store)
	require.NotNil(t, obs)

	sess := session.New(session.WithID("s1"), session.WithUserMessage("hi"))
	require.NoError(t, store.AddSession(ctx, sess))

	emitStreamingChunks(ctx, obs, sess, chunks)

	assert.Equal(t, int64(1), store.addCalls.Load(), "first chunk should INSERT")
	assert.Equal(t, int64(chunks-1), store.updateCalls.Load(), "each later chunk should UPDATE")

	finalizeStreamingMessage(ctx, obs, sess)
	// MessageAdded with an existing streaming row issues one more UpdateMessage.
	assert.Equal(t, int64(chunks), store.updateCalls.Load())
}

// TestPersistenceObserver_StreamingContentAccumulates verifies mid-stream
// persistence keeps the growing assistant text in the store.
func TestPersistenceObserver_StreamingContentAccumulates(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := session.NewInMemorySessionStore()
	obs := newPersistenceObserver(store)
	require.NotNil(t, obs)

	sess := session.New(session.WithID("s1"), session.WithUserMessage("hi"))
	require.NoError(t, store.AddSession(ctx, sess))

	obs.OnEvent(ctx, sess, AgentChoice("root", sess.ID, "hel"))
	obs.OnEvent(ctx, sess, AgentChoice("root", sess.ID, "lo"))

	reloaded, err := store.GetSession(ctx, sess.ID)
	require.NoError(t, err)
	require.Len(t, reloaded.Messages, 2) // user + streaming assistant
	require.NotNil(t, reloaded.Messages[1].Message)
	assert.Equal(t, "hello", reloaded.Messages[1].Message.Message.Content)
}

// TestPersistenceObserver_FinalPayloadAndNextTurnReset verifies the canonical
// MessageAdded payload replaces the streamed deltas in place (no extra
// AddMessage), and that a second turn on the same observer opens a new row
// while preserving the finalised first row.
func TestPersistenceObserver_FinalPayloadAndNextTurnReset(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newCountingPersistenceStore()
	obs := newPersistenceObserver(store)
	require.NotNil(t, obs)

	sess := session.New(session.WithID("s1"), session.WithUserMessage("hi"))
	require.NoError(t, store.AddSession(ctx, sess))

	// Turn one: stream deltas, then finalise with the canonical payload.
	for _, tok := range []string{"dr", "af", "t"} {
		obs.OnEvent(ctx, sess, AgentChoice("root", sess.ID, tok, "m1"))
	}
	assert.Equal(t, int64(1), store.addCalls.Load())

	obs.OnEvent(ctx, sess, MessageAdded(sess.ID, &session.Message{
		AgentName: "root",
		Message: chat.Message{
			Role:      chat.MessageRoleAssistant,
			MessageID: "m1",
			Content:   "done",
		},
	}, "root"))
	assert.Equal(t, int64(1), store.addCalls.Load(), "MessageAdded finalises the row in place, no extra AddMessage")

	reloaded, err := store.GetSession(ctx, sess.ID)
	require.NoError(t, err)
	require.Len(t, reloaded.Messages, 2) // user + assistant
	require.NotNil(t, reloaded.Messages[1].Message)
	assert.Equal(t, "done", reloaded.Messages[1].Message.Message.Content, "canonical payload replaces the streamed deltas")

	// Turn two: the same observer and session must open a fresh row.
	for _, tok := range []string{"sec", "ond"} {
		obs.OnEvent(ctx, sess, AgentChoice("root", sess.ID, tok, "m2"))
	}
	assert.Equal(t, int64(2), store.addCalls.Load(), "second turn opens a new row")

	obs.OnEvent(ctx, sess, MessageAdded(sess.ID, &session.Message{
		AgentName: "root",
		Message: chat.Message{
			Role:      chat.MessageRoleAssistant,
			MessageID: "m2",
			Content:   "final",
		},
	}, "root"))
	assert.Equal(t, int64(2), store.addCalls.Load())

	reloaded, err = store.GetSession(ctx, sess.ID)
	require.NoError(t, err)
	require.Len(t, reloaded.Messages, 3) // user + two assistant rows
	require.NotNil(t, reloaded.Messages[1].Message)
	require.NotNil(t, reloaded.Messages[2].Message)
	assert.Equal(t, "done", reloaded.Messages[1].Message.Message.Content, "first turn row preserved")
	assert.Equal(t, "final", reloaded.Messages[2].Message.Message.Content, "second turn carries only its own content")
}

// TestPersistenceObserver_ReasoningAndTextDeltasAccumulate covers a
// reasoning-first stream with interleaved reasoning/text deltas: one
// AddMessage on the first delta, one UpdateMessage per later delta, and
// Content and ReasoningContent accumulating independently in the store.
func TestPersistenceObserver_ReasoningAndTextDeltasAccumulate(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newCountingPersistenceStore()
	obs := newPersistenceObserver(store)
	require.NotNil(t, obs)

	sess := session.New(session.WithID("s1"), session.WithUserMessage("hi"))
	require.NoError(t, store.AddSession(ctx, sess))

	deltas := []Event{
		AgentChoiceReasoning("root", sess.ID, "thi", "m1"),
		AgentChoice("root", sess.ID, "hel", "m1"),
		AgentChoiceReasoning("root", sess.ID, "nk", "m1"),
		AgentChoice("root", sess.ID, "lo", "m1"),
	}
	for _, delta := range deltas {
		obs.OnEvent(ctx, sess, delta)
	}

	assert.Equal(t, int64(1), store.addCalls.Load(), "first delta should INSERT")
	assert.Equal(t, int64(3), store.updateCalls.Load(), "each later delta should UPDATE")

	reloaded, err := store.GetSession(ctx, sess.ID)
	require.NoError(t, err)
	require.Len(t, reloaded.Messages, 2) // user + one assistant row
	require.NotNil(t, reloaded.Messages[1].Message)
	assert.Equal(t, "hello", reloaded.Messages[1].Message.Message.Content)
	assert.Equal(t, "think", reloaded.Messages[1].Message.Message.ReasoningContent)
}

func BenchmarkPersistenceObserver_StreamingChunks(b *testing.B) {
	ctx := b.Context()
	obs, store := setupPersistenceObserverBench(b)

	b.ReportAllocs()
	b.ResetTimer()
	// Bound the store each iteration: UpdateMessage loads the target session
	// and scans its message list, so reusing one session grows that list with
	// b.N. Creating and deleting a session per iteration bounds both the
	// per-session scan and the retained store state.
	for i := range b.N {
		sess := session.New(session.WithID(strconv.Itoa(i)), session.WithUserMessage("hi"))
		if err := store.AddSession(ctx, sess); err != nil {
			b.Fatal(err)
		}
		emitStreamingChunks(ctx, obs, sess, streamingBenchChunks)
		finalizeStreamingMessage(ctx, obs, sess)
		if err := store.DeleteSession(ctx, sess.ID); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPersistenceObserver_StreamingChunks_SQLite(b *testing.B) {
	ctx := b.Context()
	store := openBenchSQLiteStore(b)
	obs := newPersistenceObserver(store)
	require.NotNil(b, obs)

	sess := session.New(session.WithID("bench-sqlite"), session.WithUserMessage("hi"))
	require.NoError(b, store.AddSession(ctx, sess))

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		emitStreamingChunks(ctx, obs, sess, streamingBenchChunks)
		finalizeStreamingMessage(ctx, obs, sess)
	}
}

func openBenchSQLiteStore(b *testing.B) *session.SQLiteSessionStore {
	b.Helper()
	// NewSQLiteSessionStoreFromDB logs migration INFO on every harness
	// re-invocation; discard it so -bench output stays machine-parseable.
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.DiscardHandler))
	b.Cleanup(func() { slog.SetDefault(prev) })

	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(b, err)
	b.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)

	store, err := session.NewSQLiteSessionStoreFromDB(b.Context(), db)
	require.NoError(b, err)
	b.Cleanup(func() { _ = store.Close() })
	return store
}
