package runtime

import (
	"context"
	"database/sql"
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

// countingStore wraps an in-memory store and records AddMessage / UpdateMessage
// calls so tests can pin the per-chunk persistence contract.
type countingStore struct {
	*session.InMemorySessionStore
	addCalls    atomic.Int64
	updateCalls atomic.Int64
}

func newCountingStore() *countingStore {
	return &countingStore{
		InMemorySessionStore: session.NewInMemorySessionStore().(*session.InMemorySessionStore),
	}
}

func (s *countingStore) AddMessage(ctx context.Context, sessionID string, msg *session.Message) (int64, error) {
	s.addCalls.Add(1)
	return s.InMemorySessionStore.AddMessage(ctx, sessionID, msg)
}

func (s *countingStore) UpdateMessage(ctx context.Context, messageID int64, msg *session.Message) error {
	s.updateCalls.Add(1)
	return s.InMemorySessionStore.UpdateMessage(ctx, messageID, msg)
}

func setupPersistenceObserverBench(tb testing.TB) (*PersistenceObserver, *session.InMemorySessionStore) {
	tb.Helper()
	store := session.NewInMemorySessionStore().(*session.InMemorySessionStore)
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

	store := newCountingStore()
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

func BenchmarkPersistenceObserver_StreamingChunks(b *testing.B) {
	ctx := b.Context()
	obs, store := setupPersistenceObserverBench(b)

	b.ReportAllocs()
	b.ResetTimer()
	// Bound the store each iteration: UpdateMessage scans every session's
	// messages, so a shared session (or accumulating sessions) makes ns/op
	// a function of b.N rather than a per-turn baseline.
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
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(b, err)
	b.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)

	store, err := session.NewSQLiteSessionStoreFromDB(b.Context(), db)
	require.NoError(b, err)
	b.Cleanup(func() { _ = store.Close() })
	return store
}
