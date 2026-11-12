package runtime

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestServerFallbackResetsStreamedReasoning(t *testing.T) {
	t.Parallel()
	for _, withTools := range []bool{false, true} {
		name := "no-tools"
		if withTools {
			name = "with-tools"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := newStreamBuilder().AddContent("kept text")
			b.responses = append(b.responses, chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{ReasoningContent: "DECLINED"}}}})
			if withTools {
				b.AddToolCallName("declined-tool", "shell")
			}
			b.responses = append(b.responses,
				chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{ServerFallback: &chat.ServerFallback{To: "fallback"}}}}},
				chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{ReasoningContent: "FALLBACK"}}}},
			)
			b.AddContent(" and answer").AddStopWithUsage(1, 1)
			a := agent.New("root", "test")
			sess := session.New()
			var events []Event
			result, err := handleStream(t.Context(), nil, b.Build(), a, nil, sess, EventSinkFunc(func(e Event) { events = append(events, e) }), time.Second)
			require.NoError(t, err)
			require.Equal(t, "FALLBACK", result.ReasoningContent)
			require.Equal(t, "kept text and answer", result.Content)
			require.Empty(t, result.Calls)
			resetAt, reasoningAt, resetCount := -1, -1, 0
			for i, e := range events {
				switch e := e.(type) {
				case *AgentChoiceReasoningResetEvent:
					resetAt, resetCount = i, resetCount+1
					require.Equal(t, sess.ID, e.SessionID)
					require.Equal(t, result.MessageID, e.MessageID)
					require.Equal(t, "root", e.AgentName)
				case *AgentChoiceReasoningEvent:
					if e.Content == "FALLBACK" {
						reasoningAt = i
					}
				case *ToolCallsRetractedEvent:
					require.True(t, withTools)
					require.Equal(t, []string{"declined-tool"}, e.ToolCallIDs)
				}
			}
			require.Equal(t, 1, resetCount)
			require.Greater(t, reasoningAt, resetAt)
		})
	}
}

func TestServerFallbackWithoutReasoningDoesNotReset(t *testing.T) {
	t.Parallel()
	b := newStreamBuilder().AddContent("kept")
	b.responses = append(b.responses, chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{ServerFallback: &chat.ServerFallback{To: "fallback"}}}}})
	b.AddStopWithUsage(1, 1)
	_, err := handleStream(t.Context(), nil, b.Build(), agent.New("root", "test"), []tools.Tool{}, session.New(), EventSinkFunc(func(e Event) {
		_, reset := e.(*AgentChoiceReasoningResetEvent)
		require.False(t, reset)
	}), time.Second)
	require.NoError(t, err)
}

func TestPersistenceReasoningResetBeforeCompletion(t *testing.T) {
	t.Parallel()
	store, err := sqlitestore.New(t.Context(), t.TempDir()+"/sessions.db")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	sess := session.New()
	obs := newPersistenceObserver(store)
	obs.OnRunStart(t.Context(), sess)
	emit := func(e Event) { obs.OnEvent(t.Context(), sess, e) }
	emit(AgentChoice("root", sess.ID, "kept", "answer"))
	emit(AgentChoiceReasoning("root", sess.ID, "DECLINED", "answer"))
	emit(AgentChoiceReasoningReset("root", "other-session", "answer"))
	emit(AgentChoiceReasoningReset("root", sess.ID, "other-message"))
	load := func(reasoning string) {
		t.Helper()
		saved, err := store.GetSession(t.Context(), sess.ID)
		require.NoError(t, err)
		require.Len(t, saved.OwnMessages(), 1)
		msg := saved.OwnMessages()[0].Message
		require.Equal(t, "kept", msg.Content)
		require.Equal(t, "answer", msg.MessageID)
		require.Equal(t, reasoning, msg.ReasoningContent)
	}
	load("DECLINED")
	emit(AgentChoiceReasoningReset("root", sess.ID, "answer"))
	load("") // A reload or cancellation here must not expose declined reasoning.
	emit(StreamStopped(sess.ID, "root", TurnEndReasonNormal))
	load("")
	emit(AgentChoiceReasoning("root", sess.ID, "FALLBACK", "answer"))
	load("FALLBACK")
}
