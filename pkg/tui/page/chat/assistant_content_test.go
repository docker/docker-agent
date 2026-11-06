package chat

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	chatapi "github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func newContentTestPage(t *testing.T) (*chatPage, *session.Session) {
	t.Helper()
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	sess := session.New()
	p := New(ar, t.Context(), app.New(t.Context(), queueTestRuntime{}, sess), service.NewSessionState(sess)).(*chatPage)
	t.Cleanup(func() { Cleanup(p) })
	p.messages.SetSize(100, 40)
	return p, sess
}

func TestCanonicalAssistantContentAppearsWithoutReload(t *testing.T) {
	t.Parallel()
	p, sess := newContentTestPage(t)
	msg := session.NewAgentMessage("root", &chatapi.Message{Role: chatapi.MessageRoleAssistant, MessageID: "answer", Content: "CANONICAL-FINAL-ANSWER"})
	for _, event := range []runtime.Event{runtime.StreamStarted(sess.ID, "root"), runtime.MessageAdded(sess.ID, msg, "root"), runtime.StreamStopped(sess.ID, "root", "normal")} {
		_, _ = p.handleRuntimeEvent(event)
	}
	require.Contains(t, ansi.Strip(p.messages.View()), "CANONICAL-FINAL-ANSWER")
	require.True(t, p.hasReceivedAssistantContent)
}

func TestCanonicalAssistantRepairsMissingContentWithoutDuplicates(t *testing.T) {
	t.Parallel()
	for _, partial := range []string{"FINAL-", "ANSWER", "FINAL-ANSWER"} {
		t.Run(partial, func(t *testing.T) {
			p, sess := newContentTestPage(t)
			_, _ = p.handleRuntimeEvent(runtime.StreamStarted(sess.ID, "root"))
			_, _ = p.handleRuntimeEvent(runtime.AgentChoice("root", sess.ID, partial, "answer"))
			msg := session.NewAgentMessage("root", &chatapi.Message{Role: chatapi.MessageRoleAssistant, MessageID: "answer", Content: "FINAL-ANSWER"})
			for range 2 {
				_, _ = p.handleRuntimeEvent(runtime.MessageAdded(sess.ID, msg, "root"))
			}
			_, _ = p.handleRuntimeEvent(runtime.StreamStopped(sess.ID, "root", "normal"))
			frame := ansi.Strip(p.messages.View())
			require.Equal(t, 1, strings.Count(frame, "FINAL-ANSWER"))
			require.Equal(t, 1, p.messages.MessageTypeCount(types.MessageTypeAssistant))
		})
	}
}

func TestDifferentMessageIDsKeepRetryAnswerVisible(t *testing.T) {
	t.Parallel()
	p, sess := newContentTestPage(t)
	for _, event := range []runtime.Event{
		runtime.StreamStarted(sess.ID, "root"),
		runtime.AgentChoice("root", sess.ID, "[diagnostic](", "attempt-one"),
		runtime.AgentChoice("root", sess.ID, "FINAL-ANSWER)", "attempt-two"),
		runtime.StreamStopped(sess.ID, "root", "normal"),
	} {
		_, _ = p.handleRuntimeEvent(event)
	}
	require.Contains(t, ansi.Strip(p.messages.View()), "FINAL-ANSWER")
	require.Equal(t, 2, p.messages.MessageTypeCount(types.MessageTypeAssistant))
}

func TestCanonicalAssistantDoesNotMergeWithPreviousTurn(t *testing.T) {
	t.Parallel()
	p, sess := newContentTestPage(t)
	for _, event := range []runtime.Event{
		runtime.StreamStarted(sess.ID, "root"), runtime.AgentChoice("root", sess.ID, "first"), runtime.StreamStopped(sess.ID, "root", "normal"),
		runtime.StreamStarted(sess.ID, "root"), runtime.MessageAdded(sess.ID, session.NewAgentMessage("root", &chatapi.Message{Role: chatapi.MessageRoleAssistant, Content: "second"}), "root"), runtime.StreamStopped(sess.ID, "root", "normal"),
	} {
		_, _ = p.handleRuntimeEvent(event)
	}
	require.Equal(t, 2, p.messages.MessageTypeCount(types.MessageTypeAssistant))
	require.Contains(t, ansi.Strip(p.messages.View()), "second")
}

func TestPendingSpinnerDoesNotMergeDistinctMessageIDs(t *testing.T) {
	t.Parallel()
	p, sess := newContentTestPage(t)
	_, _ = p.handleRuntimeEvent(runtime.AgentChoice("root", sess.ID, "[diagnostic](", "first"))
	p.setPendingResponse(true)
	_, _ = p.handleRuntimeEvent(runtime.AgentChoice("root", sess.ID, "FINAL-ANSWER)", "second"))
	require.Contains(t, ansi.Strip(p.messages.View()), "FINAL-ANSWER")
	require.Equal(t, 2, p.messages.MessageTypeCount(types.MessageTypeAssistant))
}

func TestCanonicalContentCannotRevealSuppressedToolXML(t *testing.T) {
	t.Parallel()
	p, sess := newContentTestPage(t)
	msg := session.NewAgentMessage("root", &chatapi.Message{Role: chatapi.MessageRoleAssistant, MessageID: "answer", Content: `safe prefix<tool_call>{"arguments":"PRIVATE-TOOL-ARGUMENT`})
	_, _ = p.handleRuntimeEvent(runtime.AgentChoice("root", sess.ID, "safe prefix", "answer"))
	_, _ = p.handleRuntimeEvent(runtime.MessageAdded(sess.ID, msg, "root"))
	require.Contains(t, ansi.Strip(p.messages.View()), "safe prefix")
	require.NotContains(t, ansi.Strip(p.messages.View()), "PRIVATE-TOOL-ARGUMENT")
	sess.AddMessage(msg)
	p.messages.LoadFromSession(sess, nil)
	require.NotContains(t, ansi.Strip(p.messages.View()), "PRIVATE-TOOL-ARGUMENT")
}

func TestRecoveredSessionClearsNestedBusyWithoutStartingQueuedRun(t *testing.T) {
	t.Parallel()
	p, sess := newContentTestPage(t)
	for _, id := range []string{sess.ID, "child", "nested"} {
		_, _ = p.handleRuntimeEvent(runtime.StreamStarted(id, "root"))
	}
	p.messageQueue = []queuedMessage{{content: "must not run"}}
	_, _ = p.handleRuntimeEvent(runtime.SessionRecovered(sess.ID))
	require.Zero(t, p.streamDepth)
	require.Empty(t, p.agentStack)
	require.False(t, p.working)
	require.Len(t, p.messageQueue, 1)
}

func TestReasoningResetRuntimeEventReachesMessageList(t *testing.T) {
	t.Parallel()
	p, sess := newContentTestPage(t)
	for _, e := range []runtime.Event{
		runtime.AgentChoiceReasoning("root", sess.ID, "DECLINED", "answer"),
		runtime.AgentChoiceReasoningReset("root", sess.ID, "answer"),
		runtime.AgentChoiceReasoning("root", sess.ID, "FALLBACK", "answer"),
		runtime.StreamStopped(sess.ID, "root", "normal"),
	} {
		handled, _ := p.handleRuntimeEvent(e)
		require.True(t, handled)
	}
	require.Equal(t, 1, p.messages.MessageTypeCount(types.MessageTypeAssistantReasoningBlock))
	require.NotContains(t, ansi.Strip(p.messages.View()), "DECLINED")
}
