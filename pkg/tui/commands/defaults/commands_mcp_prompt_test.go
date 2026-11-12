package defaults

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/commands"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

// parserForPrompt wires a single MCP-prompt item into a parser exactly as
// BuildCommandCategories does, so Parse exercises the real item produced by
// newMCPPromptItem.
func parserForPrompt(info tools.PromptInfo) *commands.Parser {
	return commands.NewParser(commands.Category{
		Name:     "MCP Prompts",
		Commands: []commands.Item{newMCPPromptItem(info.Name, info)},
	})
}

// Regression guard: MCP-prompt items must carry SlashCommand + Immediate, or
// commands.Parser.Parse never matches them and the prompt falls through as plain chat
// text instead of being invoked.
func TestMCPPromptItem_WiredForSlashDispatch(t *testing.T) {
	t.Parallel()
	item := newMCPPromptItem("summarize", tools.PromptInfo{Name: "summarize"})
	assert.Equal(t, "/summarize", item.SlashCommand)
	assert.True(t, item.Immediate)
}

// A non-empty argument string is mapped to the prompt's first declared argument.
func TestMCPPromptItem_ParseMapsArgToFirstArgument(t *testing.T) {
	t.Parallel()
	parser := parserForPrompt(tools.PromptInfo{
		Name: "summarize",
		Arguments: []tools.PromptArgument{
			{Name: "topic"},
			{Name: "tone"},
		},
	})

	cmd := parser.Parse("/summarize the quarterly report")
	require.NotNil(t, cmd)
	msg, ok := cmd().(messages.MCPPromptMsg)
	require.True(t, ok)
	assert.Equal(t, "summarize", msg.PromptName)
	assert.Equal(t, map[string]string{"topic": "the quarterly report"}, msg.Arguments)
}

// No argument and no required arguments: the prompt runs immediately with an
// empty argument map (the palette-click path).
func TestMCPPromptItem_ParseNoArgNoRequiredRunsEmpty(t *testing.T) {
	t.Parallel()
	parser := parserForPrompt(tools.PromptInfo{
		Name:      "summarize",
		Arguments: []tools.PromptArgument{{Name: "topic"}}, // optional
	})

	cmd := parser.Parse("/summarize")
	require.NotNil(t, cmd)
	msg, ok := cmd().(messages.MCPPromptMsg)
	require.True(t, ok)
	assert.Equal(t, "summarize", msg.PromptName)
	assert.Empty(t, msg.Arguments)
}

// No argument but a required argument is declared: fall back to the input
// dialog rather than invoking the prompt with a missing value.
func TestMCPPromptItem_ParseNoArgWithRequiredOpensDialog(t *testing.T) {
	t.Parallel()
	parser := parserForPrompt(tools.PromptInfo{
		Name:      "summarize",
		Arguments: []tools.PromptArgument{{Name: "topic", Required: true}},
	})

	cmd := parser.Parse("/summarize")
	require.NotNil(t, cmd)
	msg, ok := cmd().(messages.ShowMCPPromptInputMsg)
	require.True(t, ok)
	assert.Equal(t, "summarize", msg.PromptName)
}
