package completions

import (
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/tui/commands"
	"github.com/docker/docker-agent/pkg/tui/components/completion"
)

type commandCompletion struct {
	categories []commands.Category
}

func NewCommandCompletion(categories []commands.Category) Completion {
	return &commandCompletion{
		categories: categories,
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

	for _, cmd := range c.categories {
		for _, command := range cmd.Commands {
			items = append(items, completion.Item{
				Label:       command.Label,
				Description: command.Description,
				Value:       command.SlashCommand,
			})
		}
	}

	slices.SortFunc(items, func(a, b completion.Item) int {
		return strings.Compare(strings.ToLower(a.Label), strings.ToLower(b.Label))
	})
	return items
}

func (c *commandCompletion) MatchMode() completion.MatchMode {
	return completion.MatchPrefix
}

// ArgumentItems implements ArgumentCompleter: it looks up the command whose
// SlashCommand is the first token of line and, if it exposes a
// CompleteArgument provider, maps its candidates to completion items. The
// full command line ("/cmd label") is used as Value so selecting an item
// replaces the editor content wholesale — this tolerates argument values
// containing spaces, which a word-based splice would not.
func (c *commandCompletion) ArgumentItems(line string) ([]completion.Item, bool) {
	slashCommand, _, _ := strings.Cut(line, " ")

	for _, cat := range c.categories {
		for _, cmd := range cat.Commands {
			if cmd.SlashCommand != slashCommand || cmd.CompleteArgument == nil {
				continue
			}

			candidates := cmd.CompleteArgument()
			items := make([]completion.Item, 0, len(candidates))
			for _, candidate := range candidates {
				description := candidate.Description
				if candidate.Disabled {
					description = strings.TrimSpace(description + " (not restartable)")
				}
				items = append(items, completion.Item{
					Label:       candidate.Label,
					Description: description,
					Value:       cmd.SlashCommand + " " + candidate.Label,
					Disabled:    candidate.Disabled,
				})
			}
			return items, true
		}
	}

	return nil, false
}
