package dialog

import (
	"fmt"
	"strconv"
	"testing"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

func TestSessionBrowserNavigation(t *testing.T) {
	sessions := []session.Summary{
		{ID: "1", Title: "Session 1", CreatedAt: time.Now()},
		{ID: "2", Title: "Session 2", CreatedAt: time.Now()},
		{ID: "3", Title: "Session 3", CreatedAt: time.Now()},
	}

	dialog := NewSessionBrowserDialog(sessions)
	d := dialog.(*sessionBrowserDialog)

	// Initialize and set window size like the TUI does
	d.Init()
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 50})

	// Initially selected should be 0
	require.Equal(t, 0, d.selected, "initial selection should be 0")

	// Test that key bindings match correctly
	downKey := tea.KeyPressMsg{Code: tea.KeyDown}
	upKey := tea.KeyPressMsg{Code: tea.KeyUp}

	t.Logf("Down key matches keyMap.Down: %v", key.Matches(downKey, d.keyMap.Down))
	t.Logf("Up key matches keyMap.Up: %v", key.Matches(upKey, d.keyMap.Up))

	require.True(t, key.Matches(downKey, d.keyMap.Down), "down key should match keyMap.Down")
	require.True(t, key.Matches(upKey, d.keyMap.Up), "up key should match keyMap.Up")

	// Press down arrow
	updated, _ := d.Update(downKey)
	d = updated.(*sessionBrowserDialog)
	require.Equal(t, 1, d.selected, "selection should be 1 after down arrow")

	// Press down again
	updated, _ = d.Update(downKey)
	d = updated.(*sessionBrowserDialog)
	require.Equal(t, 2, d.selected, "selection should be 2 after second down arrow")

	// Press down again (should stay at 2 since we're at the end)
	updated, _ = d.Update(downKey)
	d = updated.(*sessionBrowserDialog)
	require.Equal(t, 2, d.selected, "selection should stay at 2 at end of list")

	// Press up arrow
	updated, _ = d.Update(upKey)
	d = updated.(*sessionBrowserDialog)
	require.Equal(t, 1, d.selected, "selection should be 1 after up arrow")
}

func TestSessionBrowserNavigationWithCtrl(t *testing.T) {
	sessions := []session.Summary{
		{ID: "1", Title: "Session 1", CreatedAt: time.Now()},
		{ID: "2", Title: "Session 2", CreatedAt: time.Now()},
		{ID: "3", Title: "Session 3", CreatedAt: time.Now()},
	}

	dialog := NewSessionBrowserDialog(sessions)
	d := dialog.(*sessionBrowserDialog)
	d.Init()
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 50})

	// Test ctrl+j for down
	ctrlJ := tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl}
	t.Logf("ctrl+j matches keyMap.Down: %v", key.Matches(ctrlJ, d.keyMap.Down))

	updated, _ := d.Update(ctrlJ)
	d = updated.(*sessionBrowserDialog)
	require.Equal(t, 1, d.selected, "selection should be 1 after ctrl+j")

	// Test ctrl+k for up
	ctrlK := tea.KeyPressMsg{Code: 'k', Mod: tea.ModCtrl}
	t.Logf("ctrl+k matches keyMap.Up: %v", key.Matches(ctrlK, d.keyMap.Up))

	updated, _ = d.Update(ctrlK)
	d = updated.(*sessionBrowserDialog)
	require.Equal(t, 0, d.selected, "selection should be 0 after ctrl+k")

	// Verify ctrl+y is bound to CopyID and doesn't collide with Up.
	// We only assert key matching here to avoid clipboard side-effects in tests.
	ctrlY := tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl}
	require.True(t, key.Matches(ctrlY, d.keyMap.CopyID), "ctrl+y should match keyMap.CopyID")
	require.False(t, key.Matches(ctrlY, d.keyMap.Up), "ctrl+y should not match keyMap.Up")
}

func TestSessionBrowserViewShowsSelection(t *testing.T) {
	sessions := []session.Summary{
		{ID: "1", Title: "Session 1", CreatedAt: time.Now()},
		{ID: "2", Title: "Session 2", CreatedAt: time.Now()},
		{ID: "3", Title: "Session 3", CreatedAt: time.Now()},
	}

	dialog := NewSessionBrowserDialog(sessions)
	d := dialog.(*sessionBrowserDialog)
	d.Init()
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 50})

	// Initial view should show first session selected
	view1 := d.View()
	t.Logf("Initial view (selection=0):\n%s", view1)

	// Navigate down
	downKey := tea.KeyPressMsg{Code: tea.KeyDown}
	d.Update(downKey)

	// View should now show second session selected
	view2 := d.View()
	t.Logf("After down (selection=1):\n%s", view2)

	// The views should be different
	require.NotEqual(t, view1, view2, "view should change after navigation")
}

func TestSessionBrowserFiltersEmptySessions(t *testing.T) {
	sessions := []session.Summary{
		{ID: "1", Title: "Session 1", CreatedAt: time.Now()},
		{ID: "2", Title: "", CreatedAt: time.Now()},
		{ID: "3", Title: "Session 3", CreatedAt: time.Now()},
		{ID: "4", Title: "", CreatedAt: time.Now()},
		{ID: "5", Title: "Session 5", CreatedAt: time.Now()},
	}

	dialog := NewSessionBrowserDialog(sessions)
	d := dialog.(*sessionBrowserDialog)

	// Should only have non-empty sessions
	require.Len(t, d.sessions, 3, "should have 3 non-empty sessions")
	require.Len(t, d.filtered, 3, "filtered should also have 3 sessions")

	// Verify the correct sessions are kept
	require.Equal(t, "1", d.sessions[0].ID)
	require.Equal(t, "3", d.sessions[1].ID)
	require.Equal(t, "5", d.sessions[2].ID)
}

func TestSessionBrowserAllEmptySessions(t *testing.T) {
	sessions := []session.Summary{
		{ID: "1", Title: "", CreatedAt: time.Now()},
		{ID: "2", Title: "", CreatedAt: time.Now()},
	}

	dialog := NewSessionBrowserDialog(sessions)
	d := dialog.(*sessionBrowserDialog)

	// Should have no sessions
	require.Empty(t, d.sessions, "should have 0 sessions")
	require.Empty(t, d.filtered, "filtered should also have 0 sessions")
}

func TestSessionBrowserScrolling(t *testing.T) {
	// Create more sessions than can fit in view
	sessions := make([]session.Summary, 20)
	for i := range sessions {
		sessions[i] = session.Summary{
			ID:        strconv.Itoa(i + 1),
			Title:     fmt.Sprintf("Session %d", i+1),
			CreatedAt: time.Now(),
		}
	}

	dialog := NewSessionBrowserDialog(sessions)
	d := dialog.(*sessionBrowserDialog)
	d.Init()
	// Set a small window size to force scrolling
	d.Update(tea.WindowSizeMsg{Width: 80, Height: 20})

	maxVisible := d.scrollview.VisibleHeight()
	t.Logf("Max visible items: %d", maxVisible)

	// Navigate down past the visible area
	downKey := tea.KeyPressMsg{Code: tea.KeyDown}
	for range maxVisible + 2 {
		d.Update(downKey)
	}

	// Selected should be beyond initial visible area
	require.Greater(t, d.selected, maxVisible-1, "selected should be beyond initial visible area")

	// Call View() to trigger offset adjustment (like the TUI does)
	view := d.View()

	scrollOffset := d.scrollview.ScrollOffset()
	t.Logf("Selected: %d, ScrollOffset: %d", d.selected, scrollOffset)

	// The scroll offset should have adjusted so selected is visible
	require.LessOrEqual(t, scrollOffset, d.selected, "scroll offset should be <= selected")
	require.Less(t, d.selected, scrollOffset+maxVisible, "selected should be within visible range")

	// Verify the view shows the selected session
	expectedTitle := fmt.Sprintf("Session %d", d.selected+1)
	require.Contains(t, view, expectedTitle, "view should contain selected session")
}

func TestSessionBrowserMouseClickSelectsSession(t *testing.T) {
	sessions := []session.Summary{
		{ID: "1", Title: "Session 1", CreatedAt: time.Now()},
		{ID: "2", Title: "Session 2", CreatedAt: time.Now()},
		{ID: "3", Title: "Session 3", CreatedAt: time.Now()},
	}

	dialog := NewSessionBrowserDialog(sessions)
	d := dialog.(*sessionBrowserDialog)
	d.Init()
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 50})

	// Initially selected should be 0
	require.Equal(t, 0, d.selected)

	// Get the dialog position to calculate where to click
	dialogRow, _ := d.Position()
	listStartY := dialogRow + sessionBrowserListStartY

	// Single-click on the second session (line index 1)
	clickMsg := tea.MouseClickMsg{
		X:      20,
		Y:      listStartY + 1,
		Button: tea.MouseLeft,
	}
	updated, cmd := d.Update(clickMsg)
	d = updated.(*sessionBrowserDialog)

	// Selection should have moved to session 2
	require.Equal(t, 1, d.selected, "single click should select session")
	// Single click should not produce a load command
	require.Nil(t, cmd, "single click should not trigger load")

	// Single-click on the third session
	clickMsg = tea.MouseClickMsg{
		X:      20,
		Y:      listStartY + 2,
		Button: tea.MouseLeft,
	}
	updated, cmd = d.Update(clickMsg)
	d = updated.(*sessionBrowserDialog)

	require.Equal(t, 2, d.selected, "single click should select third session")
	require.Nil(t, cmd, "single click on different session should not trigger load")
}

func TestSessionBrowserDoubleClickOpensSession(t *testing.T) {
	sessions := []session.Summary{
		{ID: "sess-1", Title: "Session 1", CreatedAt: time.Now()},
		{ID: "sess-2", Title: "Session 2", CreatedAt: time.Now()},
		{ID: "sess-3", Title: "Session 3", CreatedAt: time.Now()},
	}

	dialog := NewSessionBrowserDialog(sessions)
	d := dialog.(*sessionBrowserDialog)
	d.Init()
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 50})

	dialogRow, _ := d.Position()
	listStartY := dialogRow + sessionBrowserListStartY

	// First click selects
	clickMsg := tea.MouseClickMsg{
		X:      20,
		Y:      listStartY + 1,
		Button: tea.MouseLeft,
	}
	updated, _ := d.Update(clickMsg)
	d = updated.(*sessionBrowserDialog)
	require.Equal(t, 1, d.selected)

	// Second click on the same item (double-click) should trigger load
	updated, cmd := d.Update(clickMsg)
	d = updated.(*sessionBrowserDialog)
	require.Equal(t, 1, d.selected, "selection should stay on double-clicked session")
	require.NotNil(t, cmd, "double-click should produce a command to load the session")
}

func TestSessionBrowserClickOutsideListIgnored(t *testing.T) {
	sessions := []session.Summary{
		{ID: "1", Title: "Session 1", CreatedAt: time.Now()},
		{ID: "2", Title: "Session 2", CreatedAt: time.Now()},
	}

	dialog := NewSessionBrowserDialog(sessions)
	d := dialog.(*sessionBrowserDialog)
	d.Init()
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 50})

	// Click way outside the list area
	clickMsg := tea.MouseClickMsg{
		X:      5,
		Y:      0,
		Button: tea.MouseLeft,
	}
	updated, cmd := d.Update(clickMsg)
	d = updated.(*sessionBrowserDialog)

	// Selection should remain at 0
	require.Equal(t, 0, d.selected, "click outside list should not change selection")
	require.Nil(t, cmd, "click outside list should not produce a command")
}
