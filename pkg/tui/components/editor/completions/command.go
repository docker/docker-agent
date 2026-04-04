package completions

import (
	"context"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/tui/commands"
	"github.com/docker/docker-agent/pkg/tui/components/completion"
)

type commandCompletion struct {
	app *app.App
}

func NewCommandCompletion(a *app.App) Completion {
	return &commandCompletion{
		app: a,
	}
}

func (c *commandCompletion) RequiresEmptyEditor() bool {
	return true
}

func (c *commandCompletion) Trigger() string {
	return "/"
}

func (c *commandCompletion) Items() []completion.Item {
	var items []completion.Item

	for _, cmd := range commands.BuildCommandCategories(context.Background(), c.app) {
		for _, command := range cmd.Commands {
			items = append(items, completion.Item{
				Label:       command.Label,
				Description: command.Description,
				Value:       command.SlashCommand,
			})
		}
	}

	return sortItemsByLabel(items)
}

func sortItemsByLabel(items []completion.Item) []completion.Item {
	slices.SortFunc(items, func(a, b completion.Item) int {
		return strings.Compare(strings.ToLower(a.Label), strings.ToLower(b.Label))
	})
	return items
}

func (c *commandCompletion) MatchMode() completion.MatchMode {
	return completion.MatchPrefix
}
