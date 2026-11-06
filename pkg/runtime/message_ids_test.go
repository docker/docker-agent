package runtime

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
)

func TestPersistentMessageIDsSeparateStreamAttempts(t *testing.T) {
	t.Parallel()
	store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "session.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	sess := session.New()
	require.NoError(t, store.AddSession(t.Context(), sess))
	obs := newPersistenceObserver(store)
	for _, event := range []Event{
		userMessageEvent(chat.Message{Role: chat.MessageRoleUser, Content: "user", MessageID: "user-id"}, sess.ID, 0),
		AgentChoice("root", sess.ID, "partial", "attempt-one"),
		AgentChoiceReasoning("root", sess.ID, "think", "attempt-two"),
		AgentChoice("root", sess.ID, "answer", "attempt-two"),
		MessageAdded(sess.ID, session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, MessageID: "attempt-two", Content: "answer", ReasoningContent: "think"}), "root"),
		AgentChoice("root", sess.ID, "unfinished", "attempt-three"),
		MessageAdded(sess.ID, session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleTool, Content: "tool result"}), "root"),
	} {
		obs.OnEvent(t.Context(), sess, event)
	}
	saved, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	msgs := saved.OwnMessages()
	require.Len(t, msgs, 5)
	assert.Equal(t, "user-id", msgs[0].Message.MessageID)
	assert.Equal(t, "attempt-one", msgs[1].Message.MessageID)
	assert.Equal(t, "partial", msgs[1].Message.Content)
	assert.Equal(t, "attempt-two", msgs[2].Message.MessageID)
	assert.Equal(t, "think", msgs[2].Message.ReasoningContent)
	assert.Equal(t, "attempt-three", msgs[3].Message.MessageID)
	assert.Equal(t, "unfinished", msgs[3].Message.Content)
	assert.Equal(t, chat.MessageRoleTool, msgs[4].Message.Role)
}

func TestPersistentMessageIDsStreamEvents(t *testing.T) {
	t.Parallel()
	a := agent.New("root", "test")
	sess := session.New()
	var ids []string
	for range 2 {
		var events []Event
		stream := newStreamBuilder().AddReasoning("thinking").AddContent("hello ").AddContent("world").AddStopWithUsage(1, 1).Build()
		result, err := handleStream(t.Context(), nil, stream, a, nil, sess, EventSinkFunc(func(e Event) { events = append(events, e) }), time.Second)
		require.NoError(t, err)
		require.NotEmpty(t, result.MessageID)
		var count int
		for _, ev := range events {
			switch e := ev.(type) {
			case *AgentChoiceEvent:
				assert.Equal(t, result.MessageID, e.MessageID)
				count++
			case *AgentChoiceReasoningEvent:
				assert.Equal(t, result.MessageID, e.MessageID)
				count++
			}
		}
		assert.Equal(t, 3, count)
		ids = append(ids, result.MessageID)
	}
	assert.NotEqual(t, ids[0], ids[1])
}

func TestPersistentMessageIDsObserverStateIsRunLocal(t *testing.T) {
	t.Parallel()
	store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "session.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	sess := session.New()
	r := &LocalRuntime{observers: []func() EventObserver{func() EventObserver { return newPersistenceObserver(store) }}}
	for _, content := range []string{"one", "two"} {
		input := make(chan Event, 1)
		input <- AgentChoice("root", sess.ID, content)
		close(input)
		for range r.observe(t.Context(), sess, input) {
		}
	}
	saved, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	msgs := saved.OwnMessages()
	require.Len(t, msgs, 2)
	assert.Equal(t, "one", msgs[0].Message.Content)
	assert.Equal(t, "two", msgs[1].Message.Content)
}

func TestPersistentMessageIDsHarnessFinalization(t *testing.T) {
	t.Parallel()
	store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "session.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	sess := session.New()
	require.NoError(t, store.AddSession(t.Context(), sess))
	obs := newPersistenceObserver(store)
	obs.OnEvent(t.Context(), sess, AgentChoiceReasoning("root", sess.ID, "thought", "harness-id"))
	obs.OnEvent(t.Context(), sess, AgentChoice("root", sess.ID, "answer", "harness-id"))
	r := &LocalRuntime{now: time.Now}
	r.recordHarnessAssistantMessage(sess, agent.New("root", "test"), "answer", "thought", "harness-id", "harness", nil, 0, EventSinkFunc(func(ev Event) { obs.OnEvent(t.Context(), sess, ev) }))
	saved, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	msgs := saved.OwnMessages()
	require.Len(t, msgs, 1)
	assert.Equal(t, "harness-id", msgs[0].Message.MessageID)
	assert.Equal(t, "thought", msgs[0].Message.ReasoningContent)
}
