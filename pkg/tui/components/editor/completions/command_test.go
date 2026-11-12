package completions

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/commands"
	"github.com/docker/docker-agent/pkg/tui/components/completion"
)

// categoriesWithToolsetRestart mirrors the real "Session" category's
// "Restart Toolset" command, whose label doesn't share a prefix with its
// "/toolset-restart" slash command — the case that exposed the
// inline-completion bug.
func categoriesWithToolsetRestart() []commands.Category {
	return []commands.Category{
		{
			Name: "Session",
			Commands: []commands.Item{
				{ID: "session.exit", Label: "Exit", SlashCommand: "/exit"},
				{ID: "session.toolset.restart", Label: "Restart Toolset", SlashCommand: "/toolset-restart"},
			},
		},
	}
}

func TestCommandCompletionItems(t *testing.T) {
	t.Parallel()

	items := NewCommandCompletion(categoriesWithToolsetRestart()).Items()

	require.Len(t, items, 2)
	labels := map[string]string{}
	for _, item := range items {
		labels[item.Label] = item.Value
	}
	assert.Equal(t, "/toolset-restart", labels["Restart Toolset"])
	assert.Equal(t, "/exit", labels["Exit"])
}

// TestCommandCompletionMatchesToolsetRestartQuery exercises the full pipeline
// the editor uses: items from commandCompletion.Items(), filtered by the
// completion manager in the command completion's own MatchMode. It guards
// against a regression where typing "/toolset-restart" (trigger stripped to
// "toolset-restart") failed to surface the command because filtering only
// looked at Label ("Restart Toolset"), never at Value ("/toolset-restart").
func TestCommandCompletionMatchesToolsetRestartQuery(t *testing.T) {
	t.Parallel()

	cmdCompletion := NewCommandCompletion(categoriesWithToolsetRestart())

	for _, query := range []string{"toolset-restart", "tool", "t"} {
		t.Run(query, func(t *testing.T) {
			t.Parallel()

			m := completion.New()
			m.Update(completion.OpenMsg{
				Items:     cmdCompletion.Items(),
				MatchMode: cmdCompletion.MatchMode(),
			})
			_, cmd := m.Update(completion.QueryMsg{Query: query})
			require.NotNil(t, cmd)

			view := m.View()
			assert.Contains(t, view, "Restart Toolset", "toolset-restart command should appear for query %q", query)
		})
	}
}

func TestCommandCompletionMatchModeIsPrefix(t *testing.T) {
	t.Parallel()

	c := NewCommandCompletion(nil)
	assert.Equal(t, completion.MatchPrefix, c.MatchMode())
}

// categoriesWithArgumentCommand adds a command with a CompleteArgument
// provider (mirroring /toolset-restart) alongside one with none, so tests
// can exercise both the hit and the no-provider miss paths.
func categoriesWithArgumentCommand() []commands.Category {
	return []commands.Category{
		{
			Name: "Session",
			Commands: []commands.Item{
				{ID: "session.exit", Label: "Exit", SlashCommand: "/exit"},
				{
					ID:           "session.toolset.restart",
					Label:        "Restart Toolset",
					SlashCommand: "/toolset-restart",
					CompleteArgument: func() []commands.ArgumentCandidate {
						return []commands.ArgumentCandidate{
							{Label: "github", Description: "MCP · ready"},
							{Label: "filesystem", Description: "Built-in · ready", Disabled: true},
						}
					},
				},
			},
		},
	}
}

func TestCommandCompletionArgumentItems_Hit(t *testing.T) {
	t.Parallel()

	c, ok := NewCommandCompletion(categoriesWithArgumentCommand()).(ArgumentCompleter)
	require.True(t, ok, "commandCompletion must implement ArgumentCompleter")

	items, matched := c.ArgumentItems("/toolset-restart gi")
	require.True(t, matched)
	require.Len(t, items, 2)

	assert.Equal(t, "github", items[0].Label)
	assert.Equal(t, "/toolset-restart github", items[0].Value)
	assert.False(t, items[0].Disabled)
	assert.NotContains(t, items[0].Description, "not restartable")

	assert.Equal(t, "filesystem", items[1].Label)
	assert.Equal(t, "/toolset-restart filesystem", items[1].Value)
	assert.True(t, items[1].Disabled)
	assert.Contains(t, items[1].Description, "not restartable")
}

func TestCommandCompletionArgumentItems_UnknownCommand(t *testing.T) {
	t.Parallel()

	c, ok := NewCommandCompletion(categoriesWithArgumentCommand()).(ArgumentCompleter)
	require.True(t, ok)

	items, matched := c.ArgumentItems("/nope something")
	assert.False(t, matched)
	assert.Nil(t, items)
}

func TestCommandCompletionArgumentItems_CommandWithoutProvider(t *testing.T) {
	t.Parallel()

	c, ok := NewCommandCompletion(categoriesWithArgumentCommand()).(ArgumentCompleter)
	require.True(t, ok)

	// /exit exists but has no CompleteArgument, so it must miss just like an
	// unknown command — the popup should not open.
	items, matched := c.ArgumentItems("/exit")
	assert.False(t, matched)
	assert.Nil(t, items)
}

func TestCommandCompletionArgumentItems_NoTrailingArgumentStillMatches(t *testing.T) {
	t.Parallel()

	c, ok := NewCommandCompletion(categoriesWithArgumentCommand()).(ArgumentCompleter)
	require.True(t, ok)

	items, matched := c.ArgumentItems("/toolset-restart")
	require.True(t, matched)
	assert.Len(t, items, 2)
}

// TestCommandCompletionArgumentItems_QueriesFreshOnEachCall is a regression
// test for bug 2 (PR #3728): ArgumentItems must invoke CompleteArgument again
// on every call rather than reusing a result computed by an earlier call, so
// a single Tab always reflects the live toolset set.
func TestCommandCompletionArgumentItems_QueriesFreshOnEachCall(t *testing.T) {
	t.Parallel()

	callCount := 0
	categories := []commands.Category{
		{
			Name: "Session",
			Commands: []commands.Item{
				{
					ID:           "session.toolset.restart",
					Label:        "Restart Toolset",
					SlashCommand: "/toolset-restart",
					CompleteArgument: func() []commands.ArgumentCandidate {
						callCount++
						if callCount == 1 {
							return []commands.ArgumentCandidate{{Label: "oldtool"}}
						}
						return []commands.ArgumentCandidate{{Label: "newtool"}}
					},
				},
			},
		},
	}
	provider := NewCommandCompletion(categories)
	assert.Zero(t, callCount, "construction must not evaluate argument providers")
	provider.Items()
	assert.Zero(t, callCount, "listing commands must not evaluate argument providers")
	c, ok := provider.(ArgumentCompleter)
	require.True(t, ok)
	_, matched := c.ArgumentItems("/unknown ")
	require.False(t, matched)
	assert.Zero(t, callCount, "unmatched commands must not evaluate argument providers")

	first, matched := c.ArgumentItems("/toolset-restart ")
	require.True(t, matched)
	require.Len(t, first, 1)
	assert.Equal(t, "oldtool", first[0].Label)

	second, matched := c.ArgumentItems("/toolset-restart ")
	require.True(t, matched)
	require.Len(t, second, 1)
	assert.Equal(t, "newtool", second[0].Label, "a second call must reflect the current candidates, not replay the first")
	assert.Equal(t, 2, callCount, "CompleteArgument must be invoked again on each call")
}
