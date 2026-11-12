package commands

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

// ExecuteFunc is a function that executes a command with an optional argument.
type ExecuteFunc func(arg string) tea.Cmd

// Category represents a category of commands
type Category struct {
	Name     string
	Commands []Item
}

// ArgumentCandidate is one completable value for a command's argument,
// e.g. a toolset name for /toolset-restart. It is intentionally free of any
// completion-UI or app-domain types so pkg/tui/commands stays decoupled from
// both pkg/tui/components/completion and pkg/app.
type ArgumentCandidate struct {
	Label       string
	Description string
	// Disabled marks a candidate that is shown for context but cannot be
	// submitted (e.g. a non-restartable toolset for /toolset-restart).
	Disabled bool
}

// Item represents a single command in the palette
type Item struct {
	ID           string
	Label        string
	Description  string
	Category     string
	SlashCommand string
	Execute      ExecuteFunc
	Hidden       bool // Hidden commands work as slash commands but don't appear in the palette
	// Immediate marks commands that should run as soon as they are submitted
	// instead of being treated as ordinary queued chat input.
	Immediate bool
	// CompleteArgument, when set, returns the candidates for this command's
	// argument. Called lazily at completion-popup-open time so results
	// reflect current runtime state (e.g. toolset lifecycle). Nil for
	// commands with no argument completion.
	CompleteArgument func() []ArgumentCandidate
}

type Parser struct {
	categories []Category
}

func NewParser(categories ...Category) *Parser {
	return &Parser{
		categories: categories,
	}
}

func (p *Parser) Parse(input string) tea.Cmd {
	if input == "" || input[0] != '/' {
		return nil
	}

	// Split into command and argument
	cmd, arg, _ := strings.Cut(input, " ")

	// Search through all categories and commands
	for _, category := range p.categories {
		for _, item := range category.Commands {
			if item.SlashCommand == cmd && item.Immediate {
				return item.Execute(arg)
			}
		}
	}

	return nil
}
