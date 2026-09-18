package ui

import (
	"strings"

	"github.com/docker/docker-agent/pkg/tui/components/completion"
)

const autocompleteMaxRows = 8

// Autocomplete drives the command, scoped-argument, and file popups.
type Autocomplete struct {
	base        []Command
	all         []Command
	files       []completion.Item
	matches     []Command
	selected    int
	scopePrefix string
	mode        autocompleteMode
	Active      bool
}

type autocompleteMode int

const (
	autocompleteCommands autocompleteMode = iota
	autocompleteScoped
	autocompleteFiles
)

func NewAutocomplete() *Autocomplete {
	return &Autocomplete{}
}

func (a *Autocomplete) SetCommands(cmds []Command) {
	a.base = cmds
	a.all = cmds
	a.scopePrefix = ""
}

func (a *Autocomplete) SetFiles(files []completion.Item) {
	a.files = files
}

// SetScopedCommands temporarily replaces the command list with completions for
// an argument-taking command. prefix excludes the leading slash and includes
// any desired separator, for example "model ".
func (a *Autocomplete) SetScopedCommands(prefix string, cmds []Command) {
	a.all = cmds
	a.scopePrefix = prefix
	a.mode = autocompleteScoped
	a.selected = 0
}

// Sync recomputes the popup state from the current editor text. It returns true
// while the popup is showing.
func (a *Autocomplete) Sync(input string) bool {
	prefix := "/" + a.scopePrefix
	fileWord := fileQuery(input)
	var query string
	switch {
	case fileWord != "":
		a.mode = autocompleteFiles
		query = strings.TrimPrefix(fileWord, "@")
		items := completion.FilterItems(a.files, query, completion.MatchFuzzy)
		a.matches = make([]Command, len(items))
		for i, item := range items {
			a.matches[i] = Command{Name: item.Label, Desc: item.Description, Value: item.Value}
		}
	case a.scopePrefix != "" && strings.HasPrefix(input, prefix) && !strings.Contains(input, "\n"):
		a.mode = autocompleteScoped
		query = strings.TrimPrefix(input, prefix)
		a.matches = FilterScopedCommands(a.all, query)
	case strings.HasPrefix(input, "/") && !strings.ContainsAny(input, " \n"):
		a.mode = autocompleteCommands
		query = input[1:]
		a.matches = FilterCommands(a.all, query)
	default:
		a.deactivate()
		return false
	}
	a.Active = len(a.matches) > 0
	if a.selected >= len(a.matches) {
		a.selected = len(a.matches) - 1
	}
	if a.selected < 0 {
		a.selected = 0
	}
	return a.Active
}

func (a *Autocomplete) MoveUp() {
	if !a.Active {
		return
	}
	if a.selected > 0 {
		a.selected--
	}
}

func (a *Autocomplete) MoveDown() {
	if !a.Active {
		return
	}
	if a.selected < len(a.matches)-1 {
		a.selected++
	}
}

func (a *Autocomplete) Current() (Command, bool) {
	if !a.Active || a.selected >= len(a.matches) {
		return Command{}, false
	}
	return a.matches[a.selected], true
}

func (a *Autocomplete) Dismiss() {
	a.deactivate()
	if a.scopePrefix != "" {
		a.all = a.base
		a.scopePrefix = ""
	}
	a.mode = autocompleteCommands
}

func (a *Autocomplete) deactivate() {
	a.Active = false
	a.matches = nil
	a.selected = 0
}

func (a *Autocomplete) IsFileCompletion() bool {
	return a.mode == autocompleteFiles
}

// Completion returns the editor text for a selected completion.
func (a *Autocomplete) Completion(cmd Command) string {
	value := cmd.Value
	if value == "" {
		value = cmd.Name
	}
	return "/" + a.scopePrefix + value
}

// Render returns the popup rows (top to bottom) for the given width.
func (a *Autocomplete) Render(width int) []string {
	if !a.Active || len(a.matches) == 0 {
		return nil
	}

	start := 0
	if a.selected >= autocompleteMaxRows {
		start = a.selected - autocompleteMaxRows + 1
	}
	end := min(start+autocompleteMaxRows, len(a.matches))

	trigger := "/" + a.scopePrefix
	if a.mode == autocompleteFiles {
		trigger = "@"
	}

	nameWidth := 0
	for _, c := range a.matches {
		if w := len(trigger) + len(c.Name); w > nameWidth {
			nameWidth = w
		}
	}
	nameWidth = min(nameWidth, 24)

	var rows []string
	for i := start; i < end; i++ {
		c := a.matches[i]
		name := PadRight(trigger+c.Name, nameWidth)
		line := " " + name + "  " + c.Desc
		line = Truncate(line, width)
		if i == a.selected {
			rows = append(rows, lipglossSelected(line, width))
		} else {
			rows = append(rows, StMuted().Render(line))
		}
	}
	return rows
}

func fileQuery(input string) string {
	start := strings.LastIndexAny(input, " \t\n") + 1
	word := input[start:]
	if strings.HasPrefix(word, "@") {
		return word
	}
	return ""
}

// lipglossSelected highlights the selected popup row across the full width.
func lipglossSelected(line string, width int) string {
	padded := PadRight(line, width)
	return StAccent().Bold(true).Render(padded)
}
