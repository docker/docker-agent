package messages

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	tuiimage "github.com/docker/docker-agent/pkg/tui/image"
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

func generatedMediaTestImage(t *testing.T, name string) tuiimage.Inline {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 1))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var data bytes.Buffer
	require.NoError(t, png.Encode(&data, img))
	inline, ok := tuiimage.FromBytes(name, "image/png", data.Bytes())
	require.True(t, ok)
	return inline
}

func TestAppendAssistantMediaPreservesStreamedTextRendering(t *testing.T) {
	m := newMediaTestModel(t)
	m.AppendToLastMessage("root", "Here is your cat:")
	_ = m.View()

	inline := generatedMediaTestImage(t, "cat.png")
	unavailable := `Generated image "cat.png" is unavailable.`
	fallback := `Generated image "cat.png" saved to: /tmp/artifacts/sess/cat.png`
	m.AppendAssistantMedia("root", []types.AssistantMedia{{ID: 9, Fallback: unavailable}})

	tuiimage.SetRenderingEnabled(true)
	t.Cleanup(func() { tuiimage.SetRenderingEnabled(true) })
	view := m.View()
	plain := ansi.Strip(view)
	assert.Contains(t, plain, "Here is your cat:")
	assert.Contains(t, plain, unavailable)
	assert.NotContains(t, view, "cagent-image")

	m.UpdateAssistantMedia([]types.AssistantMedia{{ID: 9, Image: &inline, Fallback: fallback}})
	view = m.View()
	plain = ansi.Strip(view)
	assert.Contains(t, plain, "Here is your cat:")
	assert.Contains(t, plain, "cat.png")
	assert.Contains(t, view, "cagent-image")
	assert.NotContains(t, plain, "unavailable")
	assert.NotContains(t, plain, "saved to:")
	textIndex := strings.Index(view, "Here is your cat:")
	require.NotEqual(t, -1, textIndex)
	assert.Less(t, textIndex, strings.Index(view, "cagent-image"))

	tuiimage.SetRenderingEnabled(false)
	m.views[0] = m.createMessageView(m.messages[0])
	m.invalidateAllItems()
	view = m.View()
	plain = ansi.Strip(view)
	assert.Contains(t, plain, "Here is your cat:")
	assert.Contains(t, strings.ReplaceAll(plain, "\n", ""), fallback)
	assert.NotContains(t, view, "cagent-image")
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
