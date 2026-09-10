package commands

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/lifecycle"
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

	t.Run("settings command", func(t *testing.T) {
		t.Parallel()
		cmd := parser.Parse("/settings")
		require.NotNil(t, cmd)
		msg := cmd()
		_, ok := msg.(messages.OpenSettingsDialogMsg)
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

	t.Run("context command", func(t *testing.T) {
		t.Parallel()
		cmd := parser.Parse("/context")
		require.NotNil(t, cmd)
		msg := cmd()
		_, ok := msg.(messages.ShowContextDialogMsg)
		assert.True(t, ok)
	})

	t.Run("drop command with path", func(t *testing.T) {
		t.Parallel()
		cmd := parser.Parse("/drop notes.md")
		require.NotNil(t, cmd)
		msg := cmd()
		dropMsg, ok := msg.(messages.DropAttachedFileMsg)
		require.True(t, ok)
		assert.Equal(t, "notes.md", dropMsg.Path)
	})

	t.Run("drop command without path opens the context dialog", func(t *testing.T) {
		t.Parallel()
		cmd := parser.Parse("/drop")
		require.NotNil(t, cmd)
		msg := cmd()
		_, ok := msg.(messages.ShowContextDialogMsg)
		assert.True(t, ok)
	})

	t.Run("effort command with level", func(t *testing.T) {
		t.Parallel()
		cmd := parser.Parse("/effort high")
		require.NotNil(t, cmd)
		msg := cmd()
		setMsg, ok := msg.(messages.SetThinkingLevelMsg)
		require.True(t, ok)
		assert.Equal(t, "high", setMsg.Level)
	})

	t.Run("effort command without level", func(t *testing.T) {
		t.Parallel()
		cmd := parser.Parse("/effort")
		require.NotNil(t, cmd)
		msg := cmd()
		setMsg, ok := msg.(messages.SetThinkingLevelMsg)
		require.True(t, ok)
		assert.Empty(t, setMsg.Level)
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

// TestParseSlashCommand_New is a regression test for #4046: /new must
// forward its optional directory argument instead of dropping it.
func TestParseSlashCommand_New(t *testing.T) {
	t.Parallel()
	parser := newTestParser()

	parseNew := func(t *testing.T, input string) messages.NewSessionMsg {
		t.Helper()
		cmd := parser.Parse(input)
		require.NotNil(t, cmd)
		newMsg, ok := cmd().(messages.NewSessionMsg)
		require.True(t, ok, "should return NewSessionMsg")
		return newMsg
	}

	t.Run("new without argument keeps the generic behavior", func(t *testing.T) {
		t.Parallel()
		assert.Empty(t, parseNew(t, "/new").WorkingDir)
	})

	t.Run("new with directory argument", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, "/tmp/project", parseNew(t, "/new /tmp/project").WorkingDir)
	})

	t.Run("new trims whitespace around the argument", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, "/tmp/project", parseNew(t, "/new   /tmp/project  ").WorkingDir)
	})

	t.Run("new with whitespace-only argument behaves like no argument", func(t *testing.T) {
		t.Parallel()
		assert.Empty(t, parseNew(t, "/new   ").WorkingDir)
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

// TestSettingsCommandLabel guards against the palette/completion UI labeling
// /settings as "Preferences" again; the settings dialog is titled "Settings".
func TestSettingsCommandLabel(t *testing.T) {
	t.Parallel()

	var settings Item
	found := false
	for _, item := range builtInSettingsCommands() {
		if item.SlashCommand == "/settings" {
			settings = item
			found = true
			break
		}
	}
	require.True(t, found, "expected a /settings built-in command")
	assert.Equal(t, "settings.open", settings.ID)
	assert.Equal(t, "Settings", settings.Label)
}

// stubToolsetStatusSource is a minimal toolsetStatusSource for exercising
// toolsetRestartCandidates without a real *app.App.
type stubToolsetStatusSource struct {
	statuses []tools.ToolsetStatus
}

func (s stubToolsetStatusSource) CurrentAgentToolsetStatuses() []tools.ToolsetStatus {
	return s.statuses
}

func TestToolsetRestartCandidates_AllShownWithDisabledFlag(t *testing.T) {
	t.Parallel()

	source := stubToolsetStatusSource{statuses: []tools.ToolsetStatus{
		{Name: "github", Kind: "MCP", State: lifecycle.StateReady, Restartable: true},
		{Name: "filesystem", State: lifecycle.StateReady, Restartable: false},
	}}

	candidates := toolsetRestartCandidates(source)
	require.Len(t, candidates, 2)

	assert.Equal(t, "github", candidates[0].Label)
	assert.False(t, candidates[0].Disabled)
	assert.Contains(t, candidates[0].Description, "MCP")

	assert.Equal(t, "filesystem", candidates[1].Label)
	assert.True(t, candidates[1].Disabled, "non-restartable toolsets must be shown, not hidden")
	assert.Contains(t, candidates[1].Description, "Built-in")
}

func TestToolsetRestartCandidates_DedupePrefersRestartable(t *testing.T) {
	t.Parallel()

	source := stubToolsetStatusSource{statuses: []tools.ToolsetStatus{
		{Name: "dup", State: lifecycle.StateStopped, Restartable: false},
		{Name: "dup", State: lifecycle.StateReady, Restartable: true},
	}}

	candidates := toolsetRestartCandidates(source)
	require.Len(t, candidates, 1, "duplicate names must be deduplicated")
	assert.False(t, candidates[0].Disabled, "the restartable entry must win over the non-restartable duplicate")
}

func TestToolsetRestartCandidates_EmptyNameSkipped(t *testing.T) {
	t.Parallel()

	source := stubToolsetStatusSource{statuses: []tools.ToolsetStatus{{Name: "", Restartable: true}}}
	assert.Empty(t, toolsetRestartCandidates(source))
}

func TestAttachToolsetRestartCompletion(t *testing.T) {
	t.Parallel()

	items := builtInSessionCommands()
	source := stubToolsetStatusSource{statuses: []tools.ToolsetStatus{
		{Name: "github", Restartable: true},
	}}
	attachToolsetRestartCompletion(items, source)

	var restart, other *Item
	for i := range items {
		switch items[i].ID {
		case "session.toolset.restart":
			restart = &items[i]
		case "session.exit":
			other = &items[i]
		}
	}
	require.NotNil(t, restart)
	require.NotNil(t, restart.CompleteArgument, "the restart item must get a completer")
	candidates := restart.CompleteArgument()
	require.Len(t, candidates, 1)
	assert.Equal(t, "github", candidates[0].Label)

	require.NotNil(t, other)
	assert.Nil(t, other.CompleteArgument, "commands without a provider keep a nil CompleteArgument")
}

// TestAttachToolsetRestartCompletion_QueriesFreshOnEachCall is a regression
// test for bug 2 (PR #3728): the attached CompleteArgument closure must query
// the toolset status source live on every call, not capture a snapshot at
// attach time. Otherwise the first Tab after "/toolset-restart " shows
// whatever toolsets existed when the command list was built, not the
// current ones.
func TestAttachToolsetRestartCompletion_QueriesFreshOnEachCall(t *testing.T) {
	t.Parallel()

	items := builtInSessionCommands()
	source := &stubToolsetStatusSource{statuses: []tools.ToolsetStatus{
		{Name: "github", State: lifecycle.StateReady, Restartable: true},
	}}
	attachToolsetRestartCompletion(items, source)

	var restart *Item
	for i := range items {
		if items[i].ID == "session.toolset.restart" {
			restart = &items[i]
		}
	}
	require.NotNil(t, restart)

	first := restart.CompleteArgument()
	require.Len(t, first, 1)
	assert.Equal(t, "github", first[0].Label)

	// The toolset status source changes after the completer was attached
	// (e.g. the agent restarted with a different toolset set). A second call
	// must reflect that change rather than replaying the first snapshot.
	source.statuses = []tools.ToolsetStatus{
		{Name: "filesystem", State: lifecycle.StateReady, Restartable: true},
		{Name: "github", State: lifecycle.StateReady, Restartable: true},
	}

	second := restart.CompleteArgument()
	require.Len(t, second, 2, "must reflect the current toolset set, not the one captured at attach time")
	assert.Equal(t, "filesystem", second[0].Label)
	assert.Equal(t, "github", second[1].Label)
}

// stubEffortLevelsSource is a minimal effortLevelsSource for exercising
// effortCandidates without a real *app.App.
type stubEffortLevelsSource struct {
	levels []effort.Level
}

func (s *stubEffortLevelsSource) CurrentAgentThinkingLevels(context.Context) []effort.Level {
	return s.levels
}

func TestEffortCandidates_ReturnsSupportedLevelsInOrder(t *testing.T) {
	t.Parallel()

	source := &stubEffortLevelsSource{levels: []effort.Level{effort.None, effort.Low, effort.Medium, effort.High}}

	candidates := effortCandidates(t.Context(), source)
	require.Len(t, candidates, 4)
	assert.Equal(t, "none", candidates[0].Label)
	assert.Equal(t, "low", candidates[1].Label)
	assert.Equal(t, "medium", candidates[2].Label)
	assert.Equal(t, "high", candidates[3].Label)

	for _, c := range candidates {
		assert.False(t, c.Disabled, "unsupported levels are never listed, so no candidate is ever Disabled")
	}
}

func TestEffortCandidates_NoSupportedLevelsIsEmpty(t *testing.T) {
	t.Parallel()

	// Mirrors a remote runtime or a non-reasoning model, where
	// CurrentAgentThinkingLevels resolves to nil.
	assert.Empty(t, effortCandidates(t.Context(), &stubEffortLevelsSource{}))
}

func TestAttachEffortCompletion(t *testing.T) {
	t.Parallel()

	items := builtInSessionCommands()
	source := &stubEffortLevelsSource{levels: []effort.Level{effort.Low, effort.High}}
	attachEffortCompletion(items, t.Context(), source)

	var effortItem, other *Item
	for i := range items {
		switch items[i].ID {
		case "session.effort":
			effortItem = &items[i]
		case "session.exit":
			other = &items[i]
		}
	}
	require.NotNil(t, effortItem)
	require.NotNil(t, effortItem.CompleteArgument, "the effort item must get a completer")
	candidates := effortItem.CompleteArgument()
	require.Len(t, candidates, 2)
	assert.Equal(t, "low", candidates[0].Label)
	assert.Equal(t, "high", candidates[1].Label)

	require.NotNil(t, other)
	assert.Nil(t, other.CompleteArgument, "commands without a provider keep a nil CompleteArgument")
}

// TestAttachEffortCompletion_QueriesFreshOnEachCall is a regression test for
// bug 2 (PR #3728, mirrored for /effort): the attached CompleteArgument
// closure must query the effort-levels source live on every call, not
// capture a snapshot at attach time. The model (and so its supported levels)
// can change between attach time and Tab via /model.
func TestAttachEffortCompletion_QueriesFreshOnEachCall(t *testing.T) {
	t.Parallel()

	items := builtInSessionCommands()
	source := &stubEffortLevelsSource{levels: []effort.Level{effort.Low, effort.High}}
	attachEffortCompletion(items, t.Context(), source)

	var effortItem *Item
	for i := range items {
		if items[i].ID == "session.effort" {
			effortItem = &items[i]
		}
	}
	require.NotNil(t, effortItem)

	first := effortItem.CompleteArgument()
	require.Len(t, first, 2)
	assert.Equal(t, "low", first[0].Label)

	// The model switches to one with a different supported range after the
	// completer was attached (e.g. via /model). A second call must reflect
	// that change rather than replaying the first snapshot.
	source.levels = []effort.Level{effort.None, effort.Minimal, effort.Low, effort.Medium, effort.High, effort.XHigh}

	second := effortItem.CompleteArgument()
	require.Len(t, second, 6, "must reflect the current model's supported levels, not the one captured at attach time")
	assert.Equal(t, "none", second[0].Label)
	assert.Equal(t, "xhigh", second[5].Label)
}

// stubAttachedFilesSource is a minimal attachedFilesSource for exercising
// dropCandidates without a real *app.App.
type stubAttachedFilesSource struct {
	paths []string
}

func (s *stubAttachedFilesSource) AttachedFiles() []string {
	return s.paths
}

func TestDropCandidates_ReturnsAttachedFilesInOrder(t *testing.T) {
	t.Parallel()

	source := &stubAttachedFilesSource{paths: []string{"/repo/notes.md", "/repo/path with spaces.txt"}}

	candidates := dropCandidates(source)
	require.Len(t, candidates, 2)
	assert.Equal(t, "/repo/notes.md", candidates[0].Label)
	assert.False(t, candidates[0].Disabled, "every attached file is droppable")
	assert.Equal(t, "/repo/path with spaces.txt", candidates[1].Label)
	assert.False(t, candidates[1].Disabled)
}

func TestDropCandidates_NoAttachmentsIsEmpty(t *testing.T) {
	t.Parallel()

	assert.Empty(t, dropCandidates(&stubAttachedFilesSource{}))
}

func TestAttachDropCompletion(t *testing.T) {
	t.Parallel()

	items := builtInSessionCommands()
	source := &stubAttachedFilesSource{paths: []string{"/repo/notes.md"}}
	attachDropCompletion(items, source)

	var drop, other *Item
	for i := range items {
		switch items[i].ID {
		case "session.drop":
			drop = &items[i]
		case "session.exit":
			other = &items[i]
		}
	}
	require.NotNil(t, drop)
	require.NotNil(t, drop.CompleteArgument, "the drop item must get a completer")
	candidates := drop.CompleteArgument()
	require.Len(t, candidates, 1)
	assert.Equal(t, "/repo/notes.md", candidates[0].Label)

	require.NotNil(t, other)
	assert.Nil(t, other.CompleteArgument, "commands without a provider keep a nil CompleteArgument")
}

// TestAttachDropCompletion_QueriesFreshOnEachCall is a regression test for
// bug 2 (PR #3728, mirrored for /drop): the attached CompleteArgument
// closure must query the attached-files source live on every call, not
// capture a snapshot at attach time. Attachments change between /attach and
// /drop calls in the same session.
func TestAttachDropCompletion_QueriesFreshOnEachCall(t *testing.T) {
	t.Parallel()

	items := builtInSessionCommands()
	source := &stubAttachedFilesSource{paths: []string{"/repo/notes.md"}}
	attachDropCompletion(items, source)

	var drop *Item
	for i := range items {
		if items[i].ID == "session.drop" {
			drop = &items[i]
		}
	}
	require.NotNil(t, drop)

	first := drop.CompleteArgument()
	require.Len(t, first, 1)
	assert.Equal(t, "/repo/notes.md", first[0].Label)

	// A file gets attached after the completer was attached (e.g. via /attach
	// or @). A second call must reflect that change rather than replaying the
	// first snapshot.
	source.paths = []string{"/repo/notes.md", "/repo/plan.md"}

	second := drop.CompleteArgument()
	require.Len(t, second, 2, "must reflect the current attachment list, not the one captured at attach time")
	assert.Equal(t, "/repo/notes.md", second[0].Label)
	assert.Equal(t, "/repo/plan.md", second[1].Label)
}
