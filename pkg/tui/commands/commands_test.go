package commands

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParserContracts(t *testing.T) {
	t.Parallel()

	calls := 0
	execute := func(arg string) tea.Cmd {
		calls++
		return func() tea.Msg { return arg }
	}
	parser := NewParser(Category{Commands: []Item{
		{SlashCommand: "/now", Immediate: true, Execute: execute},
		{SlashCommand: "/hidden", Immediate: true, Hidden: true, Execute: execute},
		{SlashCommand: "/queued", Execute: execute},
	}})
	assert.Zero(t, calls)
	for _, input := range []string{"", "now", " /now", "/unknown", "/queued", "/now\targ"} {
		assert.Nil(t, parser.Parse(input))
	}
	assert.Zero(t, calls)
	for _, input := range []string{"/now  unchanged ", "/hidden  unchanged "} {
		cmd := parser.Parse(input)
		require.NotNil(t, cmd)
		assert.Equal(t, " unchanged ", cmd())
	}
	assert.Equal(t, 2, calls)
}
