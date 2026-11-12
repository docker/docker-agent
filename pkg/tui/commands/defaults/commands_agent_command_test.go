package defaults

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/tui/commands"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

// parserForAgentCommand wires a single agent-command item into a parser exactly
// as BuildCommandCategories does, so Parse exercises the real item produced by
// newAgentCommandItem.
func parserForAgentCommand(name string, cmd types.Command) *commands.Parser {
	return commands.NewParser(commands.Category{
		Name:     "Agent Commands",
		Commands: []commands.Item{newAgentCommandItem(name, cmd)},
	})
}

// Regression guard: agent-command items must carry SlashCommand + Immediate, or
// commands.Parser.Parse never matches them and the command falls through as plain chat
// text instead of being invoked.
func TestAgentCommandItem_WiredForSlashDispatch(t *testing.T) {
	t.Parallel()
	item := newAgentCommandItem("review", types.Command{Description: "Review the current diff"})
	assert.Equal(t, "/review", item.SlashCommand)
	assert.True(t, item.Immediate)
}

// A non-empty argument is appended to the forwarded slash command.
func TestAgentCommandItem_ParseForwardsCommandWithArg(t *testing.T) {
	t.Parallel()
	parser := parserForAgentCommand("review", types.Command{Description: "Review the current diff"})

	cmd := parser.Parse("/review the diff")
	require.NotNil(t, cmd)
	msg, ok := cmd().(messages.AgentCommandMsg)
	require.True(t, ok)
	assert.Equal(t, "/review the diff", msg.Command)
}

// With no argument, the bare slash command is forwarded.
func TestAgentCommandItem_ParseForwardsCommandNoArg(t *testing.T) {
	t.Parallel()
	parser := parserForAgentCommand("review", types.Command{Description: "Review the current diff"})

	cmd := parser.Parse("/review")
	require.NotNil(t, cmd)
	msg, ok := cmd().(messages.AgentCommandMsg)
	require.True(t, ok)
	assert.Equal(t, "/review", msg.Command)
}
