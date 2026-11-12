//go:build darwin

package defaults

import (
	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/commands"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func speakCommand() *commands.Item {
	return &commands.Item{
		ID:           "session.speak",
		Label:        "Speak",
		SlashCommand: "/speak",
		Description:  "Transcribe audio directly with OpenAI (requires OPENAI_API_KEY; Enter or Escape to stop)",
		Category:     "Session",
		Immediate:    true,
		Execute: func(string) tea.Cmd {
			return core.CmdHandler(messages.StartSpeakMsg{})
		},
	}
}
