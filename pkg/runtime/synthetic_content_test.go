package runtime

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

func TestSyntheticAssistantAnnouncesTextBeforeCanonicalMessage(t *testing.T) {
	t.Parallel()
	sess := session.New()
	a := agent.New("root", "test")
	var events []Event
	msg := &chat.Message{Role: chat.MessageRoleAssistant, Content: "FINAL-ANSWER"}
	addAgentMessage(sess, a, msg, EventSinkFunc(func(e Event) { events = append(events, e) }))
	require.Len(t, events, 2)
	choice := events[0].(*AgentChoiceEvent)
	require.Equal(t, "FINAL-ANSWER", choice.Content)
	require.NotEmpty(t, choice.MessageID)
	added := events[1].(*MessageAddedEvent)
	require.Equal(t, choice.MessageID, added.Message.Message.MessageID)
}

func TestAlreadyStreamedAssistantDoesNotAnnounceTextTwice(t *testing.T) {
	t.Parallel()
	sess := session.New()
	var events []Event
	msg := &chat.Message{Role: chat.MessageRoleAssistant, MessageID: "streamed", Content: "FINAL-ANSWER"}
	addAgentMessage(sess, agent.New("root", "test"), msg, EventSinkFunc(func(e Event) { events = append(events, e) }))
	require.Len(t, events, 1)
	require.IsType(t, &MessageAddedEvent{}, events[0])
}
