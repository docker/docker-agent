package messages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func newMediaTestModel(t *testing.T) *model {
	t.Helper()
	m := NewScrollableView(animation.NewRuntime(), 80, 24, &service.SessionState{}).(*model)
	m.SetSize(80, 24)
	return m
}

func assistantSessionItem(agent, content string) session.Item {
	return session.NewMessageItem(&session.Message{
		AgentName: agent,
		Message:   chat.Message{Role: chat.MessageRoleAssistant, Content: content},
	})
}

// TestLoadFromSession_AttachesGeneratedMediaAtPosition: restored media joins
// the assistant message built for its exact session position — not the
// newest message.
func TestLoadFromSession_AttachesGeneratedMediaAtPosition(t *testing.T) {
	t.Parallel()
	m := newMediaTestModel(t)
	sess := &session.Session{
		ID: "sess-restore",
		Messages: []session.Item{
			session.NewMessageItem(&session.Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: "draw"}}),
			assistantSessionItem("root", "Here is your cat:"),
			assistantSessionItem("root", "Anything else?"),
		},
	}
	media := map[int][]types.AssistantMedia{
		1: {{ID: 7, Fallback: `Generated image "cat.png" is unavailable.`}},
	}

	m.LoadFromSession(sess, media)

	require.Len(t, m.messages, 3)
	require.Len(t, m.messages[1].AssistantMedia, 1, "media must join the message at its session position")
	assert.EqualValues(t, 7, m.messages[1].AssistantMedia[0].ID)
	assert.Empty(t, m.messages[2].AssistantMedia, "later messages must stay media-free")
}

// TestLoadFromSession_MediaOnlyAssistantMessage: an assistant message with
// no text but restored media still becomes a visible message, mirroring
// AppendAssistantMedia's media-only turn.
func TestLoadFromSession_MediaOnlyAssistantMessage(t *testing.T) {
	t.Parallel()
	m := newMediaTestModel(t)
	sess := &session.Session{
		ID:       "sess-media-only",
		Messages: []session.Item{assistantSessionItem("root", "")},
	}

	m.LoadFromSession(sess, map[int][]types.AssistantMedia{
		0: {{ID: 3, Fallback: `Generated image "cat.png" is unavailable.`}},
	})

	require.Len(t, m.messages, 1)
	assert.Equal(t, types.MessageTypeAssistant, m.messages[0].Type)
	assert.Empty(t, m.messages[0].Content)
	require.Len(t, m.messages[0].AssistantMedia, 1)

	// Without media (e.g. no resolver capability) the empty turn stays
	// invisible, as before.
	m.LoadFromSession(sess, nil)
	assert.Empty(t, m.messages)
}

// TestUpdateAssistantMedia_ReplacesByID: a resolution result replaces its
// placeholder wherever it sits — including a non-final message — while
// zero-ID and unmatched items stay untouched.
func TestUpdateAssistantMedia_ReplacesByID(t *testing.T) {
	t.Parallel()
	m := newMediaTestModel(t)
	m.AppendAssistantMedia("root", []types.AssistantMedia{
		{ID: 1, Fallback: "placeholder one"},
		{Fallback: "legacy, final"},
	})
	m.AddUserMessage("and another")
	m.AppendAssistantMedia("root", []types.AssistantMedia{{ID: 2, Fallback: "placeholder two"}})

	m.UpdateAssistantMedia([]types.AssistantMedia{
		{ID: 1, Fallback: "resolved one"},
		{ID: 99, Fallback: "unknown id, dropped"},
	})

	require.Len(t, m.messages, 3)
	first := m.messages[0].AssistantMedia
	require.Len(t, first, 2)
	assert.Equal(t, "resolved one", first[0].Fallback, "the matching placeholder must be replaced in place")
	assert.Equal(t, "legacy, final", first[1].Fallback, "zero-ID items are final and untouched")
	assert.Equal(t, "placeholder two", m.messages[2].AssistantMedia[0].Fallback,
		"an unmatched placeholder must keep waiting for its own result")
}

// TestUpdateAssistantMedia_StaleResultIsNoOp: results whose placeholders no
// longer exist (e.g. the list was reloaded) change nothing.
func TestUpdateAssistantMedia_StaleResultIsNoOp(t *testing.T) {
	t.Parallel()
	m := newMediaTestModel(t)
	m.AppendAssistantMedia("root", []types.AssistantMedia{{ID: 5, Fallback: "placeholder"}})

	cmd := m.UpdateAssistantMedia([]types.AssistantMedia{{ID: 42, Fallback: "stale"}})

	assert.Nil(t, cmd)
	assert.Equal(t, "placeholder", m.messages[0].AssistantMedia[0].Fallback)
}
