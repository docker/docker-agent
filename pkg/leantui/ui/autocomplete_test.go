package ui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/modelpicker"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/components/completion"
)

func testCommands() []Command {
	return []Command{
		{Name: "new", Desc: "Start a new session", Kind: CmdBuiltin},
		{Name: "help", Desc: "Show help", Kind: CmdBuiltin},
		{Name: "compact", Desc: "Compact", Kind: CmdBuiltin},
		{Name: "plan", Desc: "Switch to planner", Kind: CmdAgent},
	}
}

func TestAutocompleteActivation(t *testing.T) {
	t.Parallel()
	a := NewAutocomplete()
	a.SetCommands(testCommands())

	assert.True(t, a.Sync("/ne"))
	cur, ok := a.Current()
	require.True(t, ok)
	assert.Equal(t, "new", cur.Name)

	assert.False(t, a.Sync("hello"))  // no leading slash
	assert.False(t, a.Sync("/new x")) // contains a space
	assert.False(t, a.Sync("/zzzzz")) // no matches
}

func TestAutocompleteScopedCommands(t *testing.T) {
	t.Parallel()
	a := NewAutocomplete()
	a.SetCommands(testCommands())
	gpt := runtime.ModelChoice{Name: "GPT Five", Ref: "openai/gpt-5", Provider: "openai", Model: "gpt-5"}
	sonnet := runtime.ModelChoice{Name: "Claude Sonnet", Ref: "anthropic/claude-sonnet-4-6", Provider: "anthropic", Model: "claude-sonnet-4-6"}
	a.SetScopedCommands("model ", []Command{
		{Name: gpt.Ref, MatchScore: func(query string) (int, bool) { return modelpicker.Score(gpt, query) }, Kind: CmdBuiltin},
		{Name: sonnet.Ref, MatchScore: func(query string) (int, bool) { return modelpicker.Score(sonnet, query) }, Kind: CmdBuiltin},
	})

	require.True(t, a.Sync("/model open"))
	cmd, ok := a.Current()
	require.True(t, ok)
	assert.Equal(t, "openai/gpt-5", cmd.Name)
	assert.Equal(t, "/model openai/gpt-5", a.Completion(cmd))

	require.True(t, a.Sync("/model five"))
	cmd, ok = a.Current()
	require.True(t, ok)
	assert.Equal(t, "openai/gpt-5", cmd.Name)

	a.Dismiss()
	assert.False(t, a.Sync("/model open"))
	require.True(t, a.Sync("/ne"))
	cmd, ok = a.Current()
	require.True(t, ok)
	assert.Equal(t, "new", cmd.Name)
}

func TestAutocompleteNavigation(t *testing.T) {
	t.Parallel()
	a := NewAutocomplete()
	a.SetCommands(testCommands())
	require.True(t, a.Sync("/")) // all commands match

	first, _ := a.Current()
	a.MoveDown()
	second, _ := a.Current()
	assert.NotEqual(t, first.Name, second.Name)

	a.MoveUp()
	back, _ := a.Current()
	assert.Equal(t, first.Name, back.Name)
}

func TestAutocompleteRenderWidth(t *testing.T) {
	t.Parallel()
	a := NewAutocomplete()
	a.SetCommands(testCommands())
	require.True(t, a.Sync("/"))
	rows := a.Render(60)
	assert.NotEmpty(t, rows)
	for _, r := range rows {
		assert.LessOrEqual(t, DisplayWidth(r), 60)
	}
}

func TestAutocompleteFiles(t *testing.T) {
	t.Parallel()
	a := NewAutocomplete()
	a.SetCommands(testCommands())
	a.SetFiles([]completion.Item{
		{Label: "pkg/leantui/update.go", Value: "@pkg/leantui/update.go"},
		{Label: "README.md", Value: "@README.md"},
	})

	require.True(t, a.Sync("please inspect @plu"))
	assert.True(t, a.IsFileCompletion())
	file, ok := a.Current()
	require.True(t, ok)
	assert.Equal(t, "pkg/leantui/update.go", file.Name)
	assert.Equal(t, "@pkg/leantui/update.go", file.Value)

	assert.False(t, a.Sync("email@example.com"))
	assert.False(t, a.Sync("done @pkg/leantui/update.go now"))
}

func TestEditorReplaceCurrentWord(t *testing.T) {
	t.Parallel()
	e := NewEditor("")
	e.SetText("inspect @pkg/old.go please")
	for range len(" please") {
		e.MoveLeft()
	}

	e.ReplaceCurrentWord("@pkg/new.go ")
	assert.Equal(t, "inspect @pkg/new.go  please", e.Text())
}

func TestAutocompleteBuiltinsBeforeAgent(t *testing.T) {
	t.Parallel()
	matches := FilterCommands(testCommands(), "")
	// The agent command "plan" must sort after every built-in.
	last := matches[len(matches)-1]
	assert.Equal(t, "plan", last.Name)
	assert.Equal(t, CmdAgent, last.Kind)
}
