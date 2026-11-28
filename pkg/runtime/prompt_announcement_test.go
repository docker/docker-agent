package runtime

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/team"
)

// A session run again without new input ends on something other than what
// its user wrote. That must not be announced, and stored, as the user's.
func TestRunStream_AnnouncesOnlyAUsersMessageAsThePrompt(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		tail session.Item
	}{
		{
			name: "the agent's own words",
			tail: session.NewMessageItem(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "half built"})),
		},
		{
			name: "a note the runtime left",
			tail: session.NewMessageItem(session.ImplicitUserMessage("the conversation was handed over")),
		},
		{
			name: "a summary of the conversation",
			tail: session.Item{Summary: "the user asked for a build"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "session.db"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = store.Close() })
			prov := &mockProvider{id: "test/mock-model", stream: newStreamBuilder().AddContent("and done").AddStopWithUsage(1, 1).Build()}
			root := agent.New("root", "You are a test agent", agent.WithModel(prov))
			rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)),
				WithSessionStore(store), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
			require.NoError(t, err)

			sess := session.New()
			sess.Messages = append(sess.Messages, tt.tail)

			var events []Event
			for ev := range rt.RunStream(t.Context(), sess) {
				events = append(events, ev)
			}

			assert.False(t, hasEventType(t, events, &UserMessageEvent{}), "the session ends on %s", tt.name)
			stored, err := store.GetSession(t.Context(), sess.ID)
			require.NoError(t, err)
			for _, msg := range stored.GetAllMessages() {
				assert.NotEqual(t, chat.MessageRoleUser, msg.Message.Role, "nothing but what the user wrote is stored as the user's")
			}
		})
	}
}

// The usage of a side request is no part of the conversation: one that lands
// behind the prompt before the run starts does not hide it.
func TestRunStream_AnnouncesAPromptASideRequestLandedBehind(t *testing.T) {
	t.Parallel()
	prov := &mockProvider{id: "test/mock-model", stream: newStreamBuilder().AddContent("done").AddStopWithUsage(1, 1).Build()}
	root := agent.New("root", "You are a test agent", agent.WithModel(prov))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)),
		WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	sess := session.New(session.WithUserMessage("build it"))
	sess.AddAuxiliaryUsage(&session.AuxiliaryUsage{ID: "aside", Purpose: "/btw"})

	var announced []*UserMessageEvent
	for ev := range rt.RunStream(t.Context(), sess) {
		if prompt, ok := ev.(*UserMessageEvent); ok {
			announced = append(announced, prompt)
		}
	}

	require.Len(t, announced, 1)
	assert.Equal(t, "build it", announced[0].Message)
	assert.Equal(t, 0, announced[0].SessionPosition)
}
