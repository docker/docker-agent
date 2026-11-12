//go:build darwin

package defaults

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestSpeakCommand_Description(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "Transcribe audio directly with OpenAI (requires OPENAI_API_KEY; Enter or Escape to stop)", speakCommand().Description)
}

func TestParseSlashCommand_Speak(t *testing.T) {
	t.Parallel()
	parser := newTestParser()

	cmd := parser.Parse("/speak")
	require.NotNil(t, cmd, "should return a command for /speak")

	msg := cmd()
	_, ok := msg.(messages.StartSpeakMsg)
	assert.True(t, ok, "should return StartSpeakMsg")
}
