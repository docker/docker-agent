package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

// streamCountingStore wraps a session.Store and tallies the assistant streaming
// writes that flow through AddMessage / UpdateMessage, plus the cumulative
// marshaled payload volume they carry — the same json.Marshal(msg.Message)
// the SQLite store performs per update (pkg/session/store.go UpdateMessage).
type streamCountingStore struct {
	session.Store

	addMessageCalls    int
	updateMessageCalls int
	cumulativeJSON     int64
}

func (s *streamCountingStore) AddMessage(ctx context.Context, id string, msg *session.Message) (int64, error) {
	s.addMessageCalls++
	s.tally(msg)
	return s.Store.AddMessage(ctx, id, msg)
}

func (s *streamCountingStore) UpdateMessage(ctx context.Context, sid string, id int64, msg *session.Message) error {
	s.updateMessageCalls++
	s.tally(msg)
	return s.Store.UpdateMessage(ctx, sid, id, msg)
}

func (s *streamCountingStore) tally(msg *session.Message) {
	if msg.Message.Role != chat.MessageRoleAssistant {
		return
	}
	b, err := json.Marshal(msg.Message)
	if err != nil {
		panic(err)
	}
	s.cumulativeJSON += int64(len(b))
}

func (s *streamCountingStore) writes() int { return s.addMessageCalls + s.updateMessageCalls }

// TestPersistenceObserver_ThrottlesStreamingWrites feeds a long stream of
// text deltas and bounds the store traffic: with the byte/time coalescing in
// place, writes grow with the number of checkpoints (final size / threshold),
// not with the chunk count, and the cumulative serialized payload stays
// within (writes × final size). Before the fix, every delta produced one
// full-row update: 401 writes and a quadratic ~10MB of marshaled JSON for
// this scenario.
func TestPersistenceObserver_ThrottlesStreamingWrites(t *testing.T) {
	t.Parallel()

	store := &streamCountingStore{Store: session.NewInMemorySessionStore()}
	obs := newPersistenceObserver(store)
	require.NotNil(t, obs)

	now := time.Unix(0, 0)
	obs.now = func() time.Time { return now }

	sess := session.New(session.WithID("s-throttle"), session.WithUserMessage("hi"))
	require.NoError(t, store.AddSession(t.Context(), sess))

	const chunks = 400
	const chunkSize = 128
	for range chunks {
		obs.OnEvent(t.Context(), sess, AgentChoice("root", sess.ID, strings.Repeat("x", chunkSize), "msg-1"))
		now = now.Add(10 * time.Millisecond)
	}

	finalBytes := int64(chunks * chunkSize)
	// Writes must be bounded by checkpoints, not chunks: the initial row
	// creation plus at most one update per streamingFlushMinBytes of growth
	// and one per streamingFlushInterval of wall clock (here 400×10ms).
	duration := time.Duration(chunks) * 10 * time.Millisecond
	maxWrites := 2 + int(finalBytes/streamingFlushMinBytes) + int(duration/streamingFlushInterval)
	assert.LessOrEqual(t, store.writes(), maxWrites,
		"streaming writes must be coalesced, not one per delta")

	// Cumulative marshaled bytes must stay within writes × final size.
	assert.Less(t, store.cumulativeJSON, int64(maxWrites)*finalBytes,
		"cumulative serialized payload must not grow quadratically with chunks")

	// The in-flight row must still reflect every delta after the last
	// checkpoint when finalised (covered in detail by the final-flush test).
	assert.Positive(t, store.addMessageCalls, "the streaming row must be created")
}

// TestPersistenceObserver_FlushesOnInterval verifies the time half of the
// coalescing: slow streams (few bytes over a long wall-clock window) still
// checkpoint at the flush interval, and nothing is written between
// checkpoints. Uses an injected clock so it is fully deterministic.
func TestPersistenceObserver_FlushesOnInterval(t *testing.T) {
	t.Parallel()

	store := &streamCountingStore{Store: session.NewInMemorySessionStore()}
	obs := newPersistenceObserver(store)
	require.NotNil(t, obs)

	now := time.Unix(0, 0)
	obs.now = func() time.Time { return now }

	sess := session.New(session.WithID("s-interval"), session.WithUserMessage("hi"))
	require.NoError(t, store.AddSession(t.Context(), sess))

	ctx := t.Context()
	// First delta creates the row immediately (recovery visibility),
	// subsequent small/fast deltas only dirty the state.
	obs.OnEvent(ctx, sess, AgentChoice("root", sess.ID, "a", "msg-1"))
	assert.Equal(t, 1, store.addMessageCalls)
	assert.Equal(t, 0, store.updateMessageCalls)

	for i := range 9 {
		obs.OnEvent(ctx, sess, AgentChoice("root", sess.ID, "b", "msg-1"))
		now = now.Add(10 * time.Millisecond)
		assert.Equal(t, 0, store.updateMessageCalls, "delta %d: below thresholds must not write", i)
	}

	// Crossing the flush interval with pending dirty content must flush.
	now = now.Add(streamingFlushInterval)
	obs.OnEvent(ctx, sess, AgentChoice("root", sess.ID, "c", "msg-1"))
	assert.Equal(t, 1, store.updateMessageCalls, "interval expiry must trigger a checkpoint")
}

// TestPersistenceObserver_FinalFlushPreservesContent verifies the terminal
// semantics are unchanged: MessageAddedEvent finalises the streaming row with
// the exact canonical content (text and reasoning), regardless of how much
// un-checkpointed delta content was still pending.
func TestPersistenceObserver_FinalFlushPreservesContent(t *testing.T) {
	t.Parallel()

	store := &streamCountingStore{Store: session.NewInMemorySessionStore()}
	obs := newPersistenceObserver(store)
	require.NotNil(t, obs)

	now := time.Unix(0, 0)
	obs.now = func() time.Time { return now } // frozen: deltas below byte threshold never checkpoint

	sess := session.New(session.WithID("s-final"), session.WithUserMessage("hi"))
	require.NoError(t, store.AddSession(t.Context(), sess))

	ctx := t.Context()
	const textChunks, reasoningChunks, chunkSize = 10, 5, 32
	for range textChunks {
		obs.OnEvent(ctx, sess, AgentChoice("root", sess.ID, strings.Repeat("a", chunkSize), "msg-final"))
	}
	for range reasoningChunks {
		obs.OnEvent(ctx, sess, AgentChoiceReasoning("root", sess.ID, strings.Repeat("r", chunkSize), "msg-final"))
	}
	assert.Equal(t, 0, store.updateMessageCalls, "small fast deltas must not checkpoint")

	wantContent := strings.Repeat("a", textChunks*chunkSize)
	wantReasoning := strings.Repeat("r", reasoningChunks*chunkSize)

	final := &session.Message{
		AgentName: "root",
		Message: chat.Message{
			Role:             chat.MessageRoleAssistant,
			MessageID:        "msg-final",
			Content:          wantContent,
			ReasoningContent: wantReasoning,
		},
	}
	obs.OnEvent(ctx, sess, MessageAdded(sess.ID, final, "root"))

	reloaded, err := store.GetSession(ctx, sess.ID)
	require.NoError(t, err)

	var got *session.Message
	for _, item := range reloaded.Messages {
		if item.Message != nil && item.Message.Message.Role == chat.MessageRoleAssistant {
			require.Nil(t, got, "exactly one assistant message item must be persisted")
			got = item.Message
		}
	}
	require.NotNil(t, got)
	assert.Equal(t, wantContent, got.Message.Content, "final content must be exact")
	assert.Equal(t, wantReasoning, got.Message.ReasoningContent, "final reasoning must be exact")
	assert.Equal(t, "msg-final", got.Message.MessageID)
	assert.Equal(t, 1, store.addMessageCalls)
	assert.Equal(t, 1, store.updateMessageCalls, "MessageAddedEvent must finalise the row in place")
}

// TestPersistenceObserver_FlushPendingContentOnError preserves the previous
// teardown behavior of ErrorEvent: the in-flight streaming row ends up with
// every delta received, even ones that had not been checkpointed yet.
func TestPersistenceObserver_FlushPendingContentOnError(t *testing.T) {
	t.Parallel()

	store := &streamCountingStore{Store: session.NewInMemorySessionStore()}
	obs := newPersistenceObserver(store)
	require.NotNil(t, obs)

	now := time.Unix(0, 0)
	obs.now = func() time.Time { return now }

	sess := session.New(session.WithID("s-error"), session.WithUserMessage("hi"))
	require.NoError(t, store.AddSession(t.Context(), sess))

	ctx := t.Context()
	for range 10 {
		obs.OnEvent(ctx, sess, AgentChoice("root", sess.ID, strings.Repeat("e", 32), "msg-err"))
	}

	obs.OnEvent(ctx, sess, ErrorWithCodeForSession(sess.ID, ErrorCodeModelError, "stream failed"))

	reloaded, err := store.GetSession(ctx, sess.ID)
	require.NoError(t, err)

	var got *session.Message
	for _, item := range reloaded.Messages {
		if item.Message != nil && item.Message.Message.Role == chat.MessageRoleAssistant {
			got = item.Message
		}
	}
	require.NotNil(t, got, "streaming row must survive the error")
	assert.Equal(t, strings.Repeat("e", 320), got.Message.Content,
		"pending deltas must be flushed before the row is finalised in place")
}
