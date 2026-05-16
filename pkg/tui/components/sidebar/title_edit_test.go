package sidebar

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestSidebar_TitleEditStateTransitions(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	sb := New(sessionState)

	// Initially not editing
	assert.False(t, sb.IsEditingTitle(), "should not be editing initially")

	// Begin editing
	sb.BeginTitleEdit()
	assert.True(t, sb.IsEditingTitle(), "should be editing after BeginTitleEdit")

	// Cancel editing
	sb.CancelTitleEdit()
	assert.False(t, sb.IsEditingTitle(), "should not be editing after CancelTitleEdit")

	// Begin editing again
	sb.BeginTitleEdit()
	assert.True(t, sb.IsEditingTitle(), "should be editing after second BeginTitleEdit")

	// Commit editing
	title := sb.CommitTitleEdit()
	assert.False(t, sb.IsEditingTitle(), "should not be editing after CommitTitleEdit")
	require.NotEmpty(t, title, "committed title should not be empty")
}

func TestSidebar_TitleEditPreservesInput(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	sb := New(sessionState)

	// Set initial title by simulating session load
	m := sb.(*model)
	m.sessionTitle = "Original Title"

	// Begin editing - should populate input with current title
	sb.BeginTitleEdit()

	// The input should have the original title
	assert.Equal(t, "Original Title", m.titleInput.Value())

	// Commit should return the title
	title := sb.CommitTitleEdit()
	assert.Equal(t, "Original Title", title)
}

func TestSidebar_TitleEditCancelRestoresOriginal(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	sb := New(sessionState)

	// Set initial title
	m := sb.(*model)
	m.sessionTitle = "Original Title"

	// Begin editing
	sb.BeginTitleEdit()

	// Simulate typing a new title
	m.titleInput.SetValue("New Title")

	// Cancel should not change the session title
	sb.CancelTitleEdit()
	assert.Equal(t, "Original Title", m.sessionTitle, "cancel should preserve original title")
}

func TestSidebar_HandleClickType(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	sb := New(sessionState)

	m := sb.(*model)
	m.sessionHasContent = true // Enable star visibility

	// Default PaddingLeft is 1, so coordinates need to account for this
	paddingLeft := m.layoutCfg.PaddingLeft // 1

	// In vertical mode, the title line is at verticalStarY
	// Click on the star area (adjusted x = 0-2, so raw x = 1-3)
	result, _ := sb.HandleClickType(paddingLeft+1, verticalStarY)
	assert.Equal(t, ClickStar, result, "click on star area should return ClickStar")

	// Set up a title with titleGenerated=true so ClickTitle can be returned
	m.sessionTitle = "Hi"
	m.titleGenerated = true

	// Click anywhere on the title area (after star) should return ClickTitle
	// Star "☆ " = 2 chars, so title area starts at position 2
	titleX := paddingLeft + 3 // middle of title
	result, _ = sb.HandleClickType(titleX, verticalStarY)
	assert.Equal(t, ClickTitle, result, "click on title area should return ClickTitle")

	// Click at the end (where pencil icon is) should also return ClickTitle
	pencilX := paddingLeft + 4
	result, _ = sb.HandleClickType(pencilX, verticalStarY)
	assert.Equal(t, ClickTitle, result, "click on pencil icon area should return ClickTitle")

	// Click elsewhere (wrong y)
	result, _ = sb.HandleClickType(10, 0)
	assert.Equal(t, ClickNone, result, "click elsewhere should return ClickNone")
}

func TestSidebar_TitleRegenerating(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	sb := New(sessionState)

	m := sb.(*model)
	m.sessionTitle = "Original Title"

	// Initially not regenerating
	assert.False(t, m.titleRegenerating, "should not be regenerating initially")
	assert.False(t, m.needsSpinner(), "should not need spinner initially")

	// Start regenerating
	cmd := sb.SetTitleRegenerating(true)
	assert.True(t, m.titleRegenerating, "should be regenerating after SetTitleRegenerating(true)")
	assert.True(t, m.needsSpinner(), "should need spinner when regenerating")
	// The returned command starts the spinner animation
	assert.NotNil(t, cmd, "should return a command to start the spinner")

	// Stop regenerating
	cmd = sb.SetTitleRegenerating(false)
	assert.False(t, m.titleRegenerating, "should not be regenerating after SetTitleRegenerating(false)")
	assert.False(t, m.needsSpinner(), "should not need spinner after stopping regeneration")
	assert.Nil(t, cmd, "should return nil command when stopping")
}

func TestSidebar_HandleClickType_WrappedTitle_Collapsed(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	sb := New(sessionState)

	m := sb.(*model)
	m.sessionHasContent = true
	m.titleGenerated = true
	m.mode = ModeCollapsed

	// Set a narrow width that will cause wrapping
	m.width = 10

	// Use a title long enough to wrap: "☆ " (2) + "LongTitle" (9) + " ✎" (2) = 13 chars
	m.sessionTitle = "LongTitle"

	paddingLeft := m.layoutCfg.PaddingLeft // 1

	// Title wraps to multiple lines - clicks on any title line should return ClickTitle
	titleLines := m.titleLineCount()
	assert.Greater(t, titleLines, 1, "title should wrap to multiple lines")

	// Click on line 0 (first title line) after star should return ClickTitle
	result, _ := sb.HandleClickType(paddingLeft+3, 0)
	assert.Equal(t, ClickTitle, result, "click on first title line should return ClickTitle")

	// Click on line 1 (wrapped title line) should also return ClickTitle
	result, _ = sb.HandleClickType(paddingLeft+1, 1)
	assert.Equal(t, ClickTitle, result, "click on wrapped title line should return ClickTitle")

	// Star should still be clickable on line 0
	result, _ = sb.HandleClickType(paddingLeft+1, 0)
	assert.Equal(t, ClickStar, result, "star should still be clickable on line 0")
}

func TestSidebar_HandleClickType_WrappedTitle_Vertical(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	sb := New(sessionState)

	m := sb.(*model)
	m.sessionHasContent = true
	m.titleGenerated = true
	m.mode = ModeVertical

	// Set a narrow width that will cause wrapping
	m.width = 10

	// Use a title long enough to wrap
	m.sessionTitle = "LongTitle"

	paddingLeft := m.layoutCfg.PaddingLeft // 1

	// Title wraps to multiple lines
	titleLines := m.titleLineCount()
	assert.Greater(t, titleLines, 1, "title should wrap to multiple lines")

	// In vertical mode, title starts at verticalStarY
	// Click on verticalStarY (first title line) after star should return ClickTitle
	result, _ := sb.HandleClickType(paddingLeft+3, verticalStarY)
	assert.Equal(t, ClickTitle, result, "click on first title line should return ClickTitle")

	// Click on verticalStarY+1 (wrapped title line) should also return ClickTitle
	result, _ = sb.HandleClickType(paddingLeft+1, verticalStarY+1)
	assert.Equal(t, ClickTitle, result, "click on wrapped title line should return ClickTitle")

	// Star should still be clickable on verticalStarY
	result, _ = sb.HandleClickType(paddingLeft+1, verticalStarY)
	assert.Equal(t, ClickStar, result, "star should still be clickable on verticalStarY")
}

func TestSidebar_HandleClickType_NoWrap(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	sb := New(sessionState)

	m := sb.(*model)
	m.sessionHasContent = true
	m.titleGenerated = true
	m.mode = ModeVertical

	// Use a wide enough width that title won't wrap
	m.width = 50

	// Short title that won't wrap
	m.sessionTitle = "Hi"

	paddingLeft := m.layoutCfg.PaddingLeft

	// Title should be on a single line
	titleLines := m.titleLineCount()
	assert.Equal(t, 1, titleLines, "title should be on single line when it doesn't wrap")

	// Click on the title area should return ClickTitle
	result, _ := sb.HandleClickType(paddingLeft+3, verticalStarY)
	assert.Equal(t, ClickTitle, result, "click on title should return ClickTitle")

	// Star should still be clickable
	result, _ = sb.HandleClickType(paddingLeft+1, verticalStarY)
	assert.Equal(t, ClickStar, result, "star should still be clickable")
}

func TestSidebar_HandleClickType_WorkingDir_Vertical(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	sb := New(sessionState)

	m := sb.(*model)
	m.sessionHasContent = true
	m.titleGenerated = true
	m.mode = ModeVertical
	m.width = 50
	m.sessionTitle = "Hi"
	m.workingDirectory = "~/projects/myapp"

	paddingLeft := m.layoutCfg.PaddingLeft

	// In vertical mode, working dir is at verticalStarY + titleLineCount + 1 (empty separator)
	titleLines := m.titleLineCount()
	wdY := verticalStarY + titleLines + 1

	// Click on the working directory line
	result, _ := sb.HandleClickType(paddingLeft+3, wdY)
	assert.Equal(t, ClickWorkingDir, result, "click on working dir line should return ClickWorkingDir")

	// Click on the title line should still return ClickTitle
	result, _ = sb.HandleClickType(paddingLeft+3, verticalStarY)
	assert.Equal(t, ClickTitle, result, "click on title should still return ClickTitle")

	// Click on the empty separator line should return ClickNone
	result, _ = sb.HandleClickType(paddingLeft+3, verticalStarY+titleLines)
	assert.Equal(t, ClickNone, result, "click on separator line should return ClickNone")
}

func TestSidebar_HandleClickType_WorkingDir_Collapsed(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	sb := New(sessionState)

	m := sb.(*model)
	m.sessionHasContent = true
	m.titleGenerated = true
	m.mode = ModeCollapsed
	m.width = 50
	m.sessionTitle = "Hi"
	m.workingDirectory = "~/projects/myapp"

	paddingLeft := m.layoutCfg.PaddingLeft

	// In collapsed mode, title occupies 1 line, then working dir
	titleLines := m.titleLineCount()
	assert.Equal(t, 1, titleLines, "title should be on single line")

	// Click on the working directory line (right after title)
	result, _ := sb.HandleClickType(paddingLeft+3, titleLines)
	assert.Equal(t, ClickWorkingDir, result, "click on working dir line should return ClickWorkingDir")

	// Click on the title should still return ClickTitle
	result, _ = sb.HandleClickType(paddingLeft+3, 0)
	assert.Equal(t, ClickTitle, result, "click on title should still return ClickTitle")
}

func TestSidebar_WorkingDirectory(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	sb := New(sessionState)

	m := sb.(*model)
	m.workingDirectory = "~/projects/myapp"

	assert.Equal(t, "~/projects/myapp", sb.WorkingDirectory())
}
