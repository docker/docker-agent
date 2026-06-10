package commands

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/messages"
)

func newTestParser() *Parser {
	return NewParser(
		Category{Name: "Session", Commands: builtInSessionCommands()},
		Category{Name: "Settings", Commands: builtInSettingsCommands()},
	)
}

func TestParseSlashCommand_Title(t *testing.T) {
	t.Parallel()
	parser := newTestParser()

	t.Run("title with argument sets title", func(t *testing.T) {
		t.Parallel()

		cmd := parser.Parse("/title My Custom Title")
		require.NotNil(t, cmd, "should return a command for /title with argument")

		// Execute the command and check the message type
		msg := cmd()
		setTitleMsg, ok := msg.(messages.SetSessionTitleMsg)
		require.True(t, ok, "should return SetSessionTitleMsg")
		assert.Equal(t, "My Custom Title", setTitleMsg.Title)
	})

	t.Run("title without argument regenerates", func(t *testing.T) {
		t.Parallel()

		cmd := parser.Parse("/title")
		require.NotNil(t, cmd, "should return a command for /title without argument")

		// Execute the command and check the message type
		msg := cmd()
		_, ok := msg.(messages.RegenerateTitleMsg)
		assert.True(t, ok, "should return RegenerateTitleMsg")
	})

	t.Run("title with only whitespace regenerates", func(t *testing.T) {
		t.Parallel()

		cmd := parser.Parse("/title   ")
		require.NotNil(t, cmd, "should return a command for /title with whitespace")

		// Execute the command and check the message type
		msg := cmd()
		_, ok := msg.(messages.RegenerateTitleMsg)
		assert.True(t, ok, "should return RegenerateTitleMsg for whitespace-only arg")
	})
}

func TestParseSlashCommand_OtherCommands(t *testing.T) {
	t.Parallel()
	parser := newTestParser()

	t.Run("exit command", func(t *testing.T) {
		t.Parallel()
		cmd := parser.Parse("/exit")
		require.NotNil(t, cmd)
		msg := cmd()
		_, ok := msg.(messages.ExitSessionMsg)
		assert.True(t, ok)
	})

	t.Run("new command", func(t *testing.T) {
		t.Parallel()
		cmd := parser.Parse("/new")
		require.NotNil(t, cmd)
		msg := cmd()
		_, ok := msg.(messages.NewSessionMsg)
		assert.True(t, ok)
	})

	t.Run("clear command", func(t *testing.T) {
		t.Parallel()
		cmd := parser.Parse("/clear")
		require.NotNil(t, cmd)
		msg := cmd()
		_, ok := msg.(messages.ClearSessionMsg)
		assert.True(t, ok)
	})

	t.Run("star command", func(t *testing.T) {
		t.Parallel()
		cmd := parser.Parse("/star")
		require.NotNil(t, cmd)
		msg := cmd()
		_, ok := msg.(messages.ToggleSessionStarMsg)
		assert.True(t, ok)
	})

	t.Run("undo command", func(t *testing.T) {
		t.Parallel()
		cmd := parser.Parse("/undo")
		require.NotNil(t, cmd)
		msg := cmd()
		_, ok := msg.(messages.UndoSnapshotMsg)
		assert.True(t, ok)
	})

	t.Run("snapshots command", func(t *testing.T) {
		t.Parallel()
		cmd := parser.Parse("/snapshots")
		require.NotNil(t, cmd)
		msg := cmd()
		_, ok := msg.(messages.ShowSnapshotsDialogMsg)
		assert.True(t, ok)
	})

	t.Run("skills command", func(t *testing.T) {
		t.Parallel()
		cmd := parser.Parse("/skills")
		require.NotNil(t, cmd)
		msg := cmd()
		_, ok := msg.(messages.ShowSkillsDialogMsg)
		assert.True(t, ok)
	})

	t.Run("unknown command returns nil", func(t *testing.T) {
		t.Parallel()
		cmd := parser.Parse("/unknown")
		assert.Nil(t, cmd)
	})

	t.Run("non-slash input returns nil", func(t *testing.T) {
		t.Parallel()
		cmd := parser.Parse("hello world")
		assert.Nil(t, cmd)
	})

	t.Run("empty input returns nil", func(t *testing.T) {
		t.Parallel()
		cmd := parser.Parse("")
		assert.Nil(t, cmd)
	})
}

func TestParseSlashCommand_Compact(t *testing.T) {
	t.Parallel()
	parser := newTestParser()

	t.Run("compact without argument", func(t *testing.T) {
		t.Parallel()
		cmd := parser.Parse("/compact")
		require.NotNil(t, cmd)
		msg := cmd()
		compactMsg, ok := msg.(messages.CompactSessionMsg)
		require.True(t, ok)
		assert.Empty(t, compactMsg.AdditionalPrompt)
	})

	t.Run("compact with argument", func(t *testing.T) {
		t.Parallel()
		cmd := parser.Parse("/compact focus on the API design")
		require.NotNil(t, cmd)
		msg := cmd()
		compactMsg, ok := msg.(messages.CompactSessionMsg)
		require.True(t, ok)
		assert.Equal(t, "focus on the API design", compactMsg.AdditionalPrompt)
	})
}

func TestRemoveByIDsDropsSnapshotCommands(t *testing.T) {
	t.Parallel()

	items := builtInSessionCommands()
	require.NotEmpty(t, items)

	hasID := func(items []Item, id string) bool {
		for _, it := range items {
			if it.ID == id {
				return true
			}
		}
		return false
	}

	require.True(t, hasID(items, "session.undo"))
	require.True(t, hasID(items, "session.snapshots"))

	filtered := removeByIDs(items, snapshotCommandIDs)
	assert.False(t, hasID(filtered, "session.undo"))
	assert.False(t, hasID(filtered, "session.snapshots"))
	// Other commands are untouched.
	assert.True(t, hasID(filtered, "session.exit"))
	assert.True(t, hasID(filtered, "session.new"))

	// Build a parser that mirrors the disabled-snapshots state and verify
	// that the snapshot slash commands no longer resolve.
	parser := NewParser(Category{Name: "Session", Commands: filtered})
	assert.Nil(t, parser.Parse("/undo"))
	assert.Nil(t, parser.Parse("/snapshots"))
	require.NotNil(t, parser.Parse("/exit"))
}

func TestParseSkipsItemsMarkedSkipImmediateParse(t *testing.T) {
	t.Parallel()

	parser := NewParser(Category{
		Name: "Skills",
		Commands: []Item{
			{
				SlashCommand:       "/services",
				Immediate:          true,
				SkipImmediateParse: true,
				Execute: func(string) tea.Cmd {
					return func() tea.Msg {
						return messages.SendMsg{Content: "/services", BypassQueue: true}
					}
				},
			},
		},
	})

	assert.Nil(t, parser.Parse("/services"))
	assert.Nil(t, parser.Parse("/services report"))
}

func TestParseStillMatchesImmediateItems(t *testing.T) {
	t.Parallel()

	parser := NewParser(Category{
		Name: "Agent Commands",
		Commands: []Item{
			{
				SlashCommand: "/review",
				Immediate:    true,
				Execute: func(arg string) tea.Cmd {
					return func() tea.Msg {
						return messages.AgentCommandMsg{Command: "/review " + arg}
					}
				},
			},
		},
	})

	cmd := parser.Parse("/review the diff")
	require.NotNil(t, cmd)
	msg, ok := cmd().(messages.AgentCommandMsg)
	require.True(t, ok)
	assert.Equal(t, "/review the diff", msg.Command)
}
