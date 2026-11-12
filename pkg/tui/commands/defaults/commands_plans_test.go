package defaults

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/commands"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestParseSlashCommand_Plans(t *testing.T) {
	t.Parallel()
	parser := newTestParser()

	cmd := parser.Parse("/plans")
	require.NotNil(t, cmd, "should return a command for /plans")

	msg := cmd()
	_, ok := msg.(messages.ShowPlanBrowserMsg)
	assert.True(t, ok, "should return ShowPlanBrowserMsg")
}

func TestPlansCommandRegistration(t *testing.T) {
	t.Parallel()

	var item *commands.Item
	for _, cmd := range builtInSessionCommands() {
		if cmd.ID == "session.plans" {
			item = &cmd
			break
		}
	}
	require.NotNil(t, item, "session.plans must be registered")
	assert.Equal(t, "Plans", item.Label)
	assert.Equal(t, "/plans", item.SlashCommand)
	assert.True(t, item.Immediate, "/plans must run immediately, not queue as chat input")
	assert.False(t, item.Hidden, "/plans must be discoverable in the command palette")
	assert.NotEmpty(t, item.Description)
}
