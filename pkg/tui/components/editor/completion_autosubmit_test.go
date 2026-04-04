package editor

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/components/completion"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestEditorHandlesAutoSubmit(t *testing.T) {
	t.Parallel()

	t.Run("AutoSubmit false inserts value", func(t *testing.T) {
		t.Parallel()

		e := newTestEditor("/he", "he")

		msg := completion.SelectedMsg{
			Value:      "/hello",
			AutoSubmit: false,
		}

		_, cmd := e.Update(msg)

		// Command should be nil because AutoSubmit is false
		assert.Nil(t, cmd)

		// Value should have trigger replaced with selected value and a space appended
		assert.Equal(t, "/hello ", e.textarea.Value())
	})

	t.Run("AutoSubmit true sends message", func(t *testing.T) {
		t.Parallel()

		e := newTestEditor("/he", "he")

		msg := completion.SelectedMsg{
			Value:      "/hello",
			AutoSubmit: true,
		}

		_, cmd := e.Update(msg)
		require.NotNil(t, cmd)

		// Find SendMsg
		found := false
		for _, m := range collectMsgs(cmd) {
			if sm, ok := m.(messages.SendMsg); ok {
				assert.Equal(t, "/hello", sm.Content)
				found = true
				break
			}
		}
		assert.True(t, found, "should return SendMsg")
	})

	t.Run("AutoSubmit true with Execute runs execute command", func(t *testing.T) {
		t.Parallel()

		e := newTestEditor("/he", "he")

		type testMsg struct{}
		msg := completion.SelectedMsg{
			Value:      "/hello",
			AutoSubmit: true,
			Execute: func() tea.Cmd {
				return func() tea.Msg { return testMsg{} }
			},
		}

		_, cmd := e.Update(msg)
		require.NotNil(t, cmd)

		// Execute should return the provided command
		msgs := collectMsgs(cmd)
		require.Len(t, msgs, 1)
		_, ok := msgs[0].(testMsg)
		assert.True(t, ok, "should return the command from Execute")

		// It should also clear the trigger and completion word from textarea
		assert.Empty(t, e.textarea.Value(), "should clear the trigger and completion word")
	})
}
