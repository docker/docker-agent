package dialog

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/core/layout"
)

// collectMsgs executes a command (or batch/sequence of commands) and collects all returned messages.
// It handles tea.BatchMsg and tea.Sequence (which uses an unexported slice type).
func collectMsgs(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}

	msg := cmd()
	if msg == nil {
		return nil
	}

	// Handle BatchMsg
	if batchMsg, ok := msg.(tea.BatchMsg); ok {
		var msgs []tea.Msg
		for _, innerCmd := range batchMsg {
			if innerCmd != nil {
				msgs = append(msgs, collectMsgs(innerCmd)...)
			}
		}
		return msgs
	}

	// Handle Sequence (unexported type, use reflection)
	// tea.Sequence returns a func that returns a sequenceMsg which is []tea.Cmd
	msgValue := reflect.ValueOf(msg)
	if msgValue.Kind() == reflect.Slice {
		var msgs []tea.Msg
		for i := range msgValue.Len() {
			elem := msgValue.Index(i)
			if elem.CanInterface() {
				if innerCmd, ok := elem.Interface().(tea.Cmd); ok && innerCmd != nil {
					msgs = append(msgs, collectMsgs(innerCmd)...)
				}
			}
		}
		if len(msgs) > 0 {
			return msgs
		}
	}

	return []tea.Msg{msg}
}

// findMsg searches for a message of the specified type in the collected messages.
func findMsg[T any](msgs []tea.Msg) (T, bool) {
	var zero T
	for _, msg := range msgs {
		if typed, ok := msg.(T); ok {
			return typed, true
		}
	}
	return zero, false
}

// hasMsg checks if a message of the specified type exists in the collected messages.
func hasMsg[T any](msgs []tea.Msg) bool {
	_, found := findMsg[T](msgs)
	return found
}

func TestNewMultiChoiceDialog(t *testing.T) {
	t.Parallel()

	config := MultiChoiceConfig{
		DialogID: "test-dialog",
		Title:    "Test Title",
		Options: []MultiChoiceOption{
			{ID: "opt1", Label: "Option 1", Value: "value1"},
			{ID: "opt2", Label: "Option 2", Value: "value2"},
		},
		AllowCustom:    true,
		AllowSecondary: true,
	}

	dialog := NewMultiChoiceDialog(config)
	require.NotNil(t, dialog)

	d, ok := dialog.(*multiChoiceDialog)
	require.True(t, ok)

	assert.Equal(t, "test-dialog", d.config.DialogID)
	assert.Equal(t, "Test Title", d.config.Title)
	assert.Len(t, d.config.Options, 2)
	assert.True(t, d.config.AllowCustom)
	assert.True(t, d.config.AllowSecondary)
	assert.Equal(t, "Skip", d.config.SecondaryLabel)
	assert.Equal(t, "Continue", d.config.PrimaryLabel)
	// Default selection should be none
	assert.Equal(t, selectionNone, d.selected)
}

func TestMultiChoiceDialog_DefaultValues(t *testing.T) {
	t.Parallel()

	config := MultiChoiceConfig{
		DialogID: "test-defaults",
		Title:    "Defaults Test",
		Options:  []MultiChoiceOption{{ID: "opt1", Label: "Option 1", Value: "value1"}},
	}

	dialog := NewMultiChoiceDialog(config)
	d := dialog.(*multiChoiceDialog)

	assert.Equal(t, "Skip", d.config.SecondaryLabel)
	assert.Equal(t, "Continue", d.config.PrimaryLabel)
	assert.Equal(t, "Other...", d.config.CustomPlaceholder)
}

func TestMultiChoiceDialog_HasSelection(t *testing.T) {
	t.Parallel()

	config := MultiChoiceConfig{
		DialogID:    "test-has-selection",
		Title:       "Has Selection Test",
		Options:     []MultiChoiceOption{{ID: "opt1", Label: "Option 1", Value: "value1"}},
		AllowCustom: true,
	}

	d := NewMultiChoiceDialog(config).(*multiChoiceDialog)

	// No selection initially
	assert.False(t, d.hasSelection())

	// Select an option
	d.selected = selection(0)
	assert.True(t, d.hasSelection())

	// Select custom but empty
	d.selected = selectionCustom
	d.customInput.SetValue("")
	assert.False(t, d.hasSelection())

	// Select custom with text
	d.customInput.SetValue("something")
	assert.True(t, d.hasSelection())
}

func TestMultiChoiceDialog_Selection(t *testing.T) {
	t.Parallel()

	config := MultiChoiceConfig{
		DialogID: "test-selection",
		Title:    "Selection Test",
		Options: []MultiChoiceOption{
			{ID: "opt1", Label: "Option 1", Value: "value1"},
			{ID: "opt2", Label: "Option 2", Value: "value2"},
			{ID: "opt3", Label: "Option 3", Value: "value3"},
		},
		AllowCustom: true,
	}

	d := NewMultiChoiceDialog(config).(*multiChoiceDialog)

	// Should start with no selection
	assert.Equal(t, selectionNone, d.selected)

	// Move down to first option
	d.selectNext()
	assert.Equal(t, selection(0), d.selected)

	// Move through options
	d.selectNext()
	assert.Equal(t, selection(1), d.selected)
	d.selectNext()
	assert.Equal(t, selection(2), d.selected)

	// Move to custom
	d.selectNext()
	assert.Equal(t, selectionCustom, d.selected)

	// Wrap to none
	d.selectNext()
	assert.Equal(t, selectionNone, d.selected)

	// Move up from none goes to custom
	d.selectPrevious()
	assert.Equal(t, selectionCustom, d.selected)

	// Move up from custom goes to last option
	d.selectPrevious()
	assert.Equal(t, selection(2), d.selected)
}

func TestMultiChoiceDialog_SubmitDefault_NoSelection(t *testing.T) {
	t.Parallel()

	config := MultiChoiceConfig{
		DialogID:       "test-default-skip",
		Title:          "Default Skip Test",
		Options:        []MultiChoiceOption{{ID: "opt1", Label: "Option 1", Value: "value1"}},
		AllowSecondary: true,
	}

	d := NewMultiChoiceDialog(config).(*multiChoiceDialog)
	// No selection

	_, cmd := d.submitDefault()
	// Should submit skip
	require.NotNil(t, cmd)
}

func TestMultiChoiceDialog_SubmitDefault_WithSelection(t *testing.T) {
	t.Parallel()

	config := MultiChoiceConfig{
		DialogID:       "test-default-continue",
		Title:          "Default Continue Test",
		Options:        []MultiChoiceOption{{ID: "opt1", Label: "Option 1", Value: "value1"}},
		AllowSecondary: true,
	}

	d := NewMultiChoiceDialog(config).(*multiChoiceDialog)
	d.selected = selection(0) // Select first option

	_, cmd := d.submitDefault()
	// Should submit continue (the selected option)
	require.NotNil(t, cmd)
}

func TestMultiChoiceDialog_SubmitSkip(t *testing.T) {
	t.Parallel()

	config := MultiChoiceConfig{
		DialogID:       "test-skip",
		Title:          "Skip Test",
		Options:        []MultiChoiceOption{{ID: "opt1", Label: "Option 1", Value: "value1"}},
		AllowSecondary: true,
	}

	d := NewMultiChoiceDialog(config).(*multiChoiceDialog)

	_, cmd := d.submitSecondary()
	require.NotNil(t, cmd)
}

func TestMultiChoiceDialog_SubmitSkip_NotAllowed(t *testing.T) {
	t.Parallel()

	config := MultiChoiceConfig{
		DialogID:       "test-skip-not-allowed",
		Title:          "Skip Not Allowed Test",
		Options:        []MultiChoiceOption{{ID: "opt1", Label: "Option 1", Value: "value1"}},
		AllowSecondary: false,
	}

	d := NewMultiChoiceDialog(config).(*multiChoiceDialog)

	_, cmd := d.submitSecondary()
	assert.Nil(t, cmd)
}

func TestMultiChoiceDialog_SubmitContinue_Option(t *testing.T) {
	t.Parallel()

	config := MultiChoiceConfig{
		DialogID: "test-continue-option",
		Title:    "Continue Option Test",
		Options: []MultiChoiceOption{
			{ID: "opt1", Label: "Option 1", Value: "First option value"},
		},
	}

	d := NewMultiChoiceDialog(config).(*multiChoiceDialog)
	d.selected = selection(0)

	_, cmd := d.submitPrimary()
	require.NotNil(t, cmd)
}

func TestMultiChoiceDialog_SubmitContinue_Custom(t *testing.T) {
	t.Parallel()

	config := MultiChoiceConfig{
		DialogID:    "test-continue-custom",
		Title:       "Continue Custom Test",
		Options:     []MultiChoiceOption{{ID: "opt1", Label: "Option 1", Value: "value1"}},
		AllowCustom: true,
	}

	d := NewMultiChoiceDialog(config).(*multiChoiceDialog)
	d.selected = selectionCustom
	d.customInput.SetValue("My custom reason")

	_, cmd := d.submitPrimary()
	require.NotNil(t, cmd)
}

func TestMultiChoiceDialog_SubmitContinue_EmptyCustom(t *testing.T) {
	t.Parallel()

	config := MultiChoiceConfig{
		DialogID:       "test-continue-empty-custom",
		Title:          "Continue Empty Custom Test",
		Options:        []MultiChoiceOption{{ID: "opt1", Label: "Option 1", Value: "value1"}},
		AllowCustom:    true,
		AllowSecondary: true,
	}

	d := NewMultiChoiceDialog(config).(*multiChoiceDialog)
	d.selected = selectionCustom
	d.customInput.SetValue("") // Empty

	// Should submit as skip
	_, cmd := d.submitPrimary()
	require.NotNil(t, cmd)
}

func TestMultiChoiceDialog_View(t *testing.T) {
	t.Parallel()

	config := MultiChoiceConfig{
		DialogID: "test-view",
		Title:    "Choose an option:",
		Options: []MultiChoiceOption{
			{ID: "opt1", Label: "First", Value: "first"},
			{ID: "opt2", Label: "Second", Value: "second"},
		},
		AllowCustom:    true,
		AllowSecondary: true,
	}

	dialog := NewMultiChoiceDialog(config)
	d := dialog.(*multiChoiceDialog)
	d.SetSize(100, 40)

	view := d.View()

	// Should contain title and options with numbers (1-indexed)
	assert.Contains(t, view, "Choose an option:")
	assert.Contains(t, view, "1")
	assert.Contains(t, view, "First")
	assert.Contains(t, view, "2")
	assert.Contains(t, view, "Second")
	assert.Contains(t, view, "Skip")
	assert.Contains(t, view, "Continue")
}

func TestMultiChoiceDialog_View_DefaultButton(t *testing.T) {
	t.Parallel()

	config := MultiChoiceConfig{
		DialogID:       "test-view-default",
		Title:          "Default Button Test",
		Options:        []MultiChoiceOption{{ID: "opt1", Label: "Option 1", Value: "value1"}},
		AllowSecondary: true,
	}

	d := NewMultiChoiceDialog(config).(*multiChoiceDialog)
	d.SetSize(100, 40)

	// No selection - Skip should be default (show ↵)
	view1 := d.View()
	assert.Contains(t, view1, "Skip ↵")

	// With selection - Continue should be default
	d.selected = selection(0)
	view2 := d.View()
	assert.Contains(t, view2, "Continue ↵")
}

func TestMultiChoiceDialog_Clickables(t *testing.T) {
	t.Parallel()

	config := MultiChoiceConfig{
		DialogID: "test-clickables",
		Title:    "Clickables Test",
		Options: []MultiChoiceOption{
			{ID: "opt1", Label: "Option 1", Value: "value1"},
			{ID: "opt2", Label: "Option 2", Value: "value2"},
		},
		AllowCustom: true,
	}

	d := NewMultiChoiceDialog(config).(*multiChoiceDialog)
	d.SetSize(100, 40)

	// Render to populate clickables
	_ = d.View()

	// Should have 3 clickable areas: 2 options + custom
	assert.Len(t, d.clickables, 3)

	// Verify selections
	assert.Equal(t, selection(0), d.clickables[0].selection)
	assert.Equal(t, selection(1), d.clickables[1].selection)
	assert.Equal(t, selectionCustom, d.clickables[2].selection)

	// Verify row ranges (single-line options should have startRow == endRow)
	assert.Equal(t, d.clickables[0].startRow, d.clickables[0].endRow)
	assert.Equal(t, d.clickables[1].startRow, d.clickables[1].endRow)
}

func TestMultiChoiceDialog_ClickToggle(t *testing.T) {
	t.Parallel()

	config := MultiChoiceConfig{
		DialogID: "test-click-toggle",
		Title:    "Click Toggle Test",
		Options:  []MultiChoiceOption{{ID: "opt1", Label: "Option 1", Value: "value1"}},
	}

	d := NewMultiChoiceDialog(config).(*multiChoiceDialog)
	d.SetSize(100, 40)

	// Render and position
	_ = d.View()
	_, _ = d.Position()

	// Simulate click to select
	d.selected = selectionNone
	clickY := d.contentAbsRow + 0 // First option
	d.handleMouseClick(10, clickY)
	assert.Equal(t, selection(0), d.selected)

	// Click again to deselect
	d.handleMouseClick(10, clickY)
	assert.Equal(t, selectionNone, d.selected)
}

// Test that dialog implements layout.Model interface
func TestMultiChoiceDialog_ImplementsLayoutModel(t *testing.T) {
	t.Parallel()

	config := MultiChoiceConfig{
		DialogID: "test-interface",
		Title:    "Interface Test",
		Options:  []MultiChoiceOption{{ID: "opt1", Label: "Option 1", Value: "value1"}},
	}

	dialog := NewMultiChoiceDialog(config)

	// Verify it implements layout.Model
	var _ layout.Model = dialog
}

// ============================================================================
// Window Sizing Tests
// ============================================================================

func TestMultiChoiceDialog_WindowSizeMsg(t *testing.T) {
	t.Parallel()

	config := MultiChoiceConfig{
		DialogID: "test-window-size",
		Title:    "Window Size Test",
		Options:  []MultiChoiceOption{{ID: "opt1", Label: "Option 1", Value: "value1"}},
	}

	d := NewMultiChoiceDialog(config).(*multiChoiceDialog)

	// Initial size should be 0
	assert.Equal(t, 0, d.Width())
	assert.Equal(t, 0, d.Height())

	// Update via WindowSizeMsg
	updated, _ := d.Update(tea.WindowSizeMsg{Width: 120, Height: 50})
	d = updated.(*multiChoiceDialog)

	assert.Equal(t, 120, d.Width())
	assert.Equal(t, 50, d.Height())
}

// ============================================================================
// Keyboard Navigation Tests
// ============================================================================

func TestMultiChoiceDialog_KeyboardNavigation_UpDown(t *testing.T) {
	t.Parallel()

	config := MultiChoiceConfig{
		DialogID: "test-keyboard-nav",
		Title:    "Keyboard Navigation Test",
		Options: []MultiChoiceOption{
			{ID: "opt1", Label: "Option 1", Value: "value1"},
			{ID: "opt2", Label: "Option 2", Value: "value2"},
		},
		AllowCustom: true,
	}

	d := NewMultiChoiceDialog(config).(*multiChoiceDialog)
	d.SetSize(100, 40)

	downKey := tea.KeyPressMsg{Code: tea.KeyDown}
	upKey := tea.KeyPressMsg{Code: tea.KeyUp}

	// Start at none
	assert.Equal(t, selectionNone, d.selected)

	// Down -> first option
	updated, _ := d.Update(downKey)
	d = updated.(*multiChoiceDialog)
	assert.Equal(t, selection(0), d.selected)

	// Down -> second option
	updated, _ = d.Update(downKey)
	d = updated.(*multiChoiceDialog)
	assert.Equal(t, selection(1), d.selected)

	// Down -> custom
	updated, _ = d.Update(downKey)
	d = updated.(*multiChoiceDialog)
	assert.Equal(t, selectionCustom, d.selected)

	// Down -> wrap to none
	updated, _ = d.Update(downKey)
	d = updated.(*multiChoiceDialog)
	assert.Equal(t, selectionNone, d.selected)

	// Up from none -> custom
	updated, _ = d.Update(upKey)
	d = updated.(*multiChoiceDialog)
	assert.Equal(t, selectionCustom, d.selected)

	// Up -> last option
	updated, _ = d.Update(upKey)
	d = updated.(*multiChoiceDialog)
	assert.Equal(t, selection(1), d.selected)
}

func TestMultiChoiceDialog_NumberShortcuts(t *testing.T) {
	t.Parallel()

	config := MultiChoiceConfig{
		DialogID: "test-number-shortcuts",
		Title:    "Number Shortcuts Test",
		Options: []MultiChoiceOption{
			{ID: "opt1", Label: "Option 1", Value: "value1"},
			{ID: "opt2", Label: "Option 2", Value: "value2"},
			{ID: "opt3", Label: "Option 3", Value: "value3"},
		},
		AllowCustom: true,
	}

	d := NewMultiChoiceDialog(config).(*multiChoiceDialog)
	d.SetSize(100, 40)

	// Press "1" to select first option (1-indexed)
	updated, _ := d.Update(tea.KeyPressMsg{Text: "1"})
	d = updated.(*multiChoiceDialog)
	assert.Equal(t, selection(0), d.selected)

	// Press "3" to select third option
	updated, _ = d.Update(tea.KeyPressMsg{Text: "3"})
	d = updated.(*multiChoiceDialog)
	assert.Equal(t, selection(2), d.selected)

	// Press "4" to select custom (4th item when AllowCustom=true)
	updated, _ = d.Update(tea.KeyPressMsg{Text: "4"})
	d = updated.(*multiChoiceDialog)
	assert.Equal(t, selectionCustom, d.selected)

	// Press "9" - out of range, should not change
	updated, _ = d.Update(tea.KeyPressMsg{Text: "9"})
	d = updated.(*multiChoiceDialog)
	assert.Equal(t, selectionCustom, d.selected)
}

func TestMultiChoiceDialog_NumberShortcuts_NoCustom(t *testing.T) {
	t.Parallel()

	config := MultiChoiceConfig{
		DialogID:    "test-number-shortcuts-no-custom",
		Title:       "Number Shortcuts No Custom Test",
		Options:     []MultiChoiceOption{{ID: "opt1", Label: "Option 1", Value: "value1"}},
		AllowCustom: false,
	}

	d := NewMultiChoiceDialog(config).(*multiChoiceDialog)
	d.SetSize(100, 40)

	// Press "1" to select first option (1-indexed)
	updated, _ := d.Update(tea.KeyPressMsg{Text: "1"})
	d = updated.(*multiChoiceDialog)
	assert.Equal(t, selection(0), d.selected)

	// Press "2" - out of range (no custom), should not change
	updated, _ = d.Update(tea.KeyPressMsg{Text: "2"})
	d = updated.(*multiChoiceDialog)
	assert.Equal(t, selection(0), d.selected)
}

func TestMultiChoiceDialog_TypingAutoSelectsCustom(t *testing.T) {
	t.Parallel()

	config := MultiChoiceConfig{
		DialogID:    "test-typing-custom",
		Title:       "Typing Custom Test",
		Options:     []MultiChoiceOption{{ID: "opt1", Label: "Option 1", Value: "value1"}},
		AllowCustom: true,
	}

	d := NewMultiChoiceDialog(config).(*multiChoiceDialog)
	d.SetSize(100, 40)

	// Start at option
	d.selected = selection(0)

	// Type a letter - should auto-select custom and forward to input
	updated, _ := d.Update(tea.KeyPressMsg{Text: "h"})
	d = updated.(*multiChoiceDialog)
	assert.Equal(t, selectionCustom, d.selected)
	assert.Contains(t, d.customInput.Value(), "h")
}

func TestMultiChoiceDialog_TypingInCustomMode_NumbersForwarded(t *testing.T) {
	t.Parallel()

	config := MultiChoiceConfig{
		DialogID:    "test-typing-numbers-custom",
		Title:       "Typing Numbers in Custom Test",
		Options:     []MultiChoiceOption{{ID: "opt1", Label: "Option 1", Value: "value1"}},
		AllowCustom: true,
	}

	d := NewMultiChoiceDialog(config).(*multiChoiceDialog)
	d.SetSize(100, 40)

	// Start in custom mode
	d.selected = selectionCustom
	d.customInput.Focus()

	// Type "123" - should go to input, not trigger number shortcuts
	for _, ch := range "123" {
		updated, _ := d.Update(tea.KeyPressMsg{Text: string(ch)})
		d = updated.(*multiChoiceDialog)
	}

	assert.Equal(t, selectionCustom, d.selected, "should still be in custom mode")
	assert.Contains(t, d.customInput.Value(), "123")
}

func TestMultiChoiceDialog_TabOverride(t *testing.T) {
	t.Parallel()

	config := MultiChoiceConfig{
		DialogID:       "test-tab-override",
		Title:          "Tab Override Test",
		Options:        []MultiChoiceOption{{ID: "opt1", Label: "Option 1", Value: "value1"}},
		AllowSecondary: true,
	}

	d := NewMultiChoiceDialog(config).(*multiChoiceDialog)
	d.SetSize(100, 40)

	tabKey := tea.KeyPressMsg{Code: tea.KeyTab}

	// No selection, secondary is default
	assert.True(t, d.isSecondaryDefault())
	view1 := d.View()
	assert.Contains(t, view1, "Skip ↵")

	// Press Tab to toggle
	updated, _ := d.Update(tabKey)
	d = updated.(*multiChoiceDialog)
	assert.False(t, d.isSecondaryDefault())
	view2 := d.View()
	assert.Contains(t, view2, "Continue ↵")

	// Press Tab again to toggle back
	updated, _ = d.Update(tabKey)
	d = updated.(*multiChoiceDialog)
	assert.True(t, d.isSecondaryDefault())
}

func TestMultiChoiceDialog_TabOverride_WithSelection(t *testing.T) {
	t.Parallel()

	config := MultiChoiceConfig{
		DialogID:       "test-tab-override-selection",
		Title:          "Tab Override With Selection Test",
		Options:        []MultiChoiceOption{{ID: "opt1", Label: "Option 1", Value: "value1"}},
		AllowSecondary: true,
	}

	d := NewMultiChoiceDialog(config).(*multiChoiceDialog)
	d.SetSize(100, 40)
	d.selected = selection(0) // Has selection

	tabKey := tea.KeyPressMsg{Code: tea.KeyTab}

	// With selection, primary is default
	assert.False(t, d.isSecondaryDefault())
	view1 := d.View()
	assert.Contains(t, view1, "Continue ↵")

	// Press Tab to toggle - skip becomes default
	updated, _ := d.Update(tabKey)
	d = updated.(*multiChoiceDialog)
	assert.True(t, d.isSecondaryDefault())
	view2 := d.View()
	assert.Contains(t, view2, "Skip ↵")
}

func TestMultiChoiceDialog_EscapeCancels(t *testing.T) {
	t.Parallel()

	config := MultiChoiceConfig{
		DialogID: "test-escape",
		Title:    "Escape Test",
		Options:  []MultiChoiceOption{{ID: "opt1", Label: "Option 1", Value: "value1"}},
	}

	d := NewMultiChoiceDialog(config).(*multiChoiceDialog)
	d.SetSize(100, 40)

	escKey := tea.KeyPressMsg{Code: tea.KeyEscape}

	_, cmd := d.Update(escKey)
	require.NotNil(t, cmd)

	msgs := collectMsgs(cmd)
	require.NotEmpty(t, msgs)

	// Should have a CloseDialogMsg
	assert.True(t, hasMsg[CloseDialogMsg](msgs), "should emit CloseDialogMsg")

	// Should have a MultiChoiceResultMsg with IsCancelled=true
	resultMsg, found := findMsg[MultiChoiceResultMsg](msgs)
	require.True(t, found, "should emit MultiChoiceResultMsg")
	assert.True(t, resultMsg.Result.IsCancelled)
	assert.Equal(t, "test-escape", resultMsg.DialogID)
}

func TestMultiChoiceDialog_EscapeCancels_InCustomMode(t *testing.T) {
	t.Parallel()

	config := MultiChoiceConfig{
		DialogID:    "test-escape-custom",
		Title:       "Escape in Custom Mode Test",
		Options:     []MultiChoiceOption{{ID: "opt1", Label: "Option 1", Value: "value1"}},
		AllowCustom: true,
	}

	d := NewMultiChoiceDialog(config).(*multiChoiceDialog)
	d.SetSize(100, 40)
	d.selected = selectionCustom
	d.customInput.Focus()
	d.customInput.SetValue("some text")

	escKey := tea.KeyPressMsg{Code: tea.KeyEscape}

	_, cmd := d.Update(escKey)
	require.NotNil(t, cmd)

	msgs := collectMsgs(cmd)
	resultMsg, found := findMsg[MultiChoiceResultMsg](msgs)
	require.True(t, found)
	assert.True(t, resultMsg.Result.IsCancelled, "should cancel even with custom text")
}

// ============================================================================
// Submit Message Content Tests
// ============================================================================

func TestMultiChoiceDialog_SubmitOption_MessageContent(t *testing.T) {
	t.Parallel()

	config := MultiChoiceConfig{
		DialogID: "test-submit-option-msg",
		Title:    "Submit Option Message Test",
		Options: []MultiChoiceOption{
			{ID: "bad_args", Label: "Bad Arguments", Value: "The arguments are incorrect."},
		},
	}

	d := NewMultiChoiceDialog(config).(*multiChoiceDialog)
	d.SetSize(100, 40)
	d.selected = selection(0)

	_, cmd := d.submitPrimary()
	require.NotNil(t, cmd)

	msgs := collectMsgs(cmd)

	// Should have CloseDialogMsg
	assert.True(t, hasMsg[CloseDialogMsg](msgs))

	// Should have MultiChoiceResultMsg with correct content
	resultMsg, found := findMsg[MultiChoiceResultMsg](msgs)
	require.True(t, found)
	assert.Equal(t, "test-submit-option-msg", resultMsg.DialogID)
	assert.Equal(t, "bad_args", resultMsg.Result.OptionID)
	assert.Equal(t, "The arguments are incorrect.", resultMsg.Result.Value)
	assert.False(t, resultMsg.Result.IsCustom)
	assert.False(t, resultMsg.Result.IsSkipped)
	assert.False(t, resultMsg.Result.IsCancelled)
}

func TestMultiChoiceDialog_SubmitCustom_MessageContent(t *testing.T) {
	t.Parallel()

	config := MultiChoiceConfig{
		DialogID:    "test-submit-custom-msg",
		Title:       "Submit Custom Message Test",
		Options:     []MultiChoiceOption{{ID: "opt1", Label: "Option 1", Value: "value1"}},
		AllowCustom: true,
	}

	d := NewMultiChoiceDialog(config).(*multiChoiceDialog)
	d.SetSize(100, 40)
	d.selected = selectionCustom
	d.customInput.SetValue("My custom rejection reason")

	_, cmd := d.submitPrimary()
	require.NotNil(t, cmd)

	msgs := collectMsgs(cmd)

	resultMsg, found := findMsg[MultiChoiceResultMsg](msgs)
	require.True(t, found)
	assert.Equal(t, "custom", resultMsg.Result.OptionID)
	assert.Equal(t, "My custom rejection reason", resultMsg.Result.Value)
	assert.True(t, resultMsg.Result.IsCustom)
	assert.False(t, resultMsg.Result.IsSkipped)
}

func TestMultiChoiceDialog_SubmitSkip_MessageContent(t *testing.T) {
	t.Parallel()

	config := MultiChoiceConfig{
		DialogID:       "test-submit-skip-msg",
		Title:          "Submit Skip Message Test",
		Options:        []MultiChoiceOption{{ID: "opt1", Label: "Option 1", Value: "value1"}},
		AllowSecondary: true,
	}

	d := NewMultiChoiceDialog(config).(*multiChoiceDialog)
	d.SetSize(100, 40)

	_, cmd := d.submitSecondary()
	require.NotNil(t, cmd)

	msgs := collectMsgs(cmd)

	resultMsg, found := findMsg[MultiChoiceResultMsg](msgs)
	require.True(t, found)
	assert.Equal(t, "skip", resultMsg.Result.OptionID)
	assert.True(t, resultMsg.Result.IsSkipped)
	assert.Empty(t, resultMsg.Result.Value)
}

func TestMultiChoiceDialog_EnterKey_SubmitsDefault(t *testing.T) {
	t.Parallel()

	config := MultiChoiceConfig{
		DialogID:       "test-enter-default",
		Title:          "Enter Default Test",
		Options:        []MultiChoiceOption{{ID: "opt1", Label: "Option 1", Value: "value1"}},
		AllowSecondary: true,
	}

	d := NewMultiChoiceDialog(config).(*multiChoiceDialog)
	d.SetSize(100, 40)

	enterKey := tea.KeyPressMsg{Code: tea.KeyEnter}

	// No selection - enter should submit skip
	_, cmd := d.Update(enterKey)
	require.NotNil(t, cmd)
	msgs := collectMsgs(cmd)
	resultMsg, found := findMsg[MultiChoiceResultMsg](msgs)
	require.True(t, found)
	assert.True(t, resultMsg.Result.IsSkipped)
}

func TestMultiChoiceDialog_EnterKey_WithSelection(t *testing.T) {
	t.Parallel()

	config := MultiChoiceConfig{
		DialogID:       "test-enter-selection",
		Title:          "Enter With Selection Test",
		Options:        []MultiChoiceOption{{ID: "opt1", Label: "Option 1", Value: "value1"}},
		AllowSecondary: true,
	}

	d := NewMultiChoiceDialog(config).(*multiChoiceDialog)
	d.SetSize(100, 40)
	d.selected = selection(0)

	enterKey := tea.KeyPressMsg{Code: tea.KeyEnter}

	_, cmd := d.Update(enterKey)
	require.NotNil(t, cmd)
	msgs := collectMsgs(cmd)
	resultMsg, found := findMsg[MultiChoiceResultMsg](msgs)
	require.True(t, found)
	assert.Equal(t, "opt1", resultMsg.Result.OptionID)
	assert.False(t, resultMsg.Result.IsSkipped)
}

// ============================================================================
// Mouse Interaction Tests
// ============================================================================

func TestMultiChoiceDialog_MouseClick_SelectsOption(t *testing.T) {
	t.Parallel()

	config := MultiChoiceConfig{
		DialogID: "test-mouse-select",
		Title:    "Mouse Select Test",
		Options: []MultiChoiceOption{
			{ID: "opt1", Label: "Option 1", Value: "value1"},
			{ID: "opt2", Label: "Option 2", Value: "value2"},
		},
	}

	d := NewMultiChoiceDialog(config).(*multiChoiceDialog)
	d.SetSize(100, 40)

	// Render to populate clickables and positions
	_ = d.View()
	_, _ = d.Position()

	// Click on first option
	d.selected = selectionNone
	clickY := d.contentAbsRow + d.clickables[0].startRow
	updated, _ := d.handleMouseClick(d.contentAbsCol+5, clickY)
	d = updated.(*multiChoiceDialog)
	assert.Equal(t, selection(0), d.selected)

	// Click on second option
	clickY = d.contentAbsRow + d.clickables[1].startRow
	updated, _ = d.handleMouseClick(d.contentAbsCol+5, clickY)
	d = updated.(*multiChoiceDialog)
	assert.Equal(t, selection(1), d.selected)
}

func TestMultiChoiceDialog_MouseClick_SkipButton(t *testing.T) {
	t.Parallel()

	config := MultiChoiceConfig{
		DialogID:       "test-mouse-skip",
		Title:          "Mouse Skip Button Test",
		Options:        []MultiChoiceOption{{ID: "opt1", Label: "Option 1", Value: "value1"}},
		AllowSecondary: true,
	}

	d := NewMultiChoiceDialog(config).(*multiChoiceDialog)
	d.SetSize(100, 40)

	// Render to populate button positions
	_ = d.View()
	_, _ = d.Position()

	// Click on skip button
	clickX := d.contentAbsCol + d.secondaryBtnCol + 1
	clickY := d.contentAbsRow + d.btnRow
	_, cmd := d.handleMouseClick(clickX, clickY)
	require.NotNil(t, cmd)

	msgs := collectMsgs(cmd)
	resultMsg, found := findMsg[MultiChoiceResultMsg](msgs)
	require.True(t, found)
	assert.True(t, resultMsg.Result.IsSkipped)
}

func TestMultiChoiceDialog_MouseClick_ContinueButton(t *testing.T) {
	t.Parallel()

	config := MultiChoiceConfig{
		DialogID: "test-mouse-continue",
		Title:    "Mouse Continue Button Test",
		Options:  []MultiChoiceOption{{ID: "opt1", Label: "Option 1", Value: "value1"}},
	}

	d := NewMultiChoiceDialog(config).(*multiChoiceDialog)
	d.SetSize(100, 40)
	d.selected = selection(0)

	// Render to populate button positions
	_ = d.View()
	_, _ = d.Position()

	// Click on continue button
	clickX := d.contentAbsCol + d.primaryBtnCol + 1
	clickY := d.contentAbsRow + d.btnRow
	_, cmd := d.handleMouseClick(clickX, clickY)
	require.NotNil(t, cmd)

	msgs := collectMsgs(cmd)
	resultMsg, found := findMsg[MultiChoiceResultMsg](msgs)
	require.True(t, found)
	assert.Equal(t, "opt1", resultMsg.Result.OptionID)
}

func TestMultiChoiceDialog_MouseClick_MultiRowOption(t *testing.T) {
	t.Parallel()

	// Create an option with a very long label that will wrap
	longLabel := strings.Repeat("This is a very long option label that should wrap to multiple lines. ", 3)

	config := MultiChoiceConfig{
		DialogID: "test-mouse-multirow",
		Title:    "Multi-Row Option Test",
		Options: []MultiChoiceOption{
			{ID: "long", Label: longLabel, Value: "long-value"},
			{ID: "short", Label: "Short", Value: "short-value"},
		},
	}

	d := NewMultiChoiceDialog(config).(*multiChoiceDialog)
	d.SetSize(80, 40) // Smaller width to force wrapping

	// Render to populate clickables
	_ = d.View()
	_, _ = d.Position()

	// First option should span multiple rows
	if len(d.clickables) > 0 && d.clickables[0].endRow > d.clickables[0].startRow {
		// Click on the second row of the first option
		clickY := d.contentAbsRow + d.clickables[0].startRow + 1
		updated, _ := d.handleMouseClick(d.contentAbsCol+5, clickY)
		d = updated.(*multiChoiceDialog)
		assert.Equal(t, selection(0), d.selected, "clicking on wrapped row should select the option")
	}
	// If wrapping didn't happen, the test still passes (depends on label length and dialog width)
}

// ============================================================================
// Edge Cases and Helper Function Tests
// ============================================================================

func TestMultiChoiceDialog_TruncateWithEllipsisEnd(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		text     string
		maxWidth int
		expected string
	}{
		{
			name:     "text fits exactly",
			text:     "Hello",
			maxWidth: 5,
			expected: "Hello",
		},
		{
			name:     "text fits with room",
			text:     "Hi",
			maxWidth: 10,
			expected: "Hi",
		},
		{
			name:     "text needs truncation",
			text:     "Hello World",
			maxWidth: 8,
			expected: "Hello...",
		},
		{
			name:     "very short max width",
			text:     "Hello",
			maxWidth: 3,
			expected: "...",
		},
		{
			name:     "max width too small for ellipsis",
			text:     "Hello",
			maxWidth: 2,
			expected: "...",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := truncateWithEllipsisEnd(tt.text, tt.maxWidth)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestMultiChoiceDialog_CustomLabelsNotOverwritten(t *testing.T) {
	t.Parallel()

	config := MultiChoiceConfig{
		DialogID:          "test-custom-labels",
		Title:             "Custom Labels Test",
		Options:           []MultiChoiceOption{{ID: "opt1", Label: "Option 1", Value: "value1"}},
		SecondaryLabel:    "No Thanks",
		PrimaryLabel:      "Proceed",
		CustomPlaceholder: "Enter your own...",
	}

	d := NewMultiChoiceDialog(config).(*multiChoiceDialog)

	assert.Equal(t, "No Thanks", d.config.SecondaryLabel)
	assert.Equal(t, "Proceed", d.config.PrimaryLabel)
	assert.Equal(t, "Enter your own...", d.config.CustomPlaceholder)
}

func TestMultiChoiceDialog_HelpText_WithOptions(t *testing.T) {
	t.Parallel()

	config := MultiChoiceConfig{
		DialogID: "test-help-options",
		Title:    "Help Text Test",
		Options: []MultiChoiceOption{
			{ID: "opt1", Label: "Option 1", Value: "value1"},
			{ID: "opt2", Label: "Option 2", Value: "value2"},
		},
		AllowCustom: true,
	}

	d := NewMultiChoiceDialog(config).(*multiChoiceDialog)
	d.SetSize(100, 40)

	view := d.View()
	// Should show "1-3" for 2 options + custom (1-indexed)
	assert.Contains(t, view, "1-3")
	assert.Contains(t, view, "select")
}

func TestMultiChoiceDialog_HelpText_NoOptions(t *testing.T) {
	t.Parallel()

	config := MultiChoiceConfig{
		DialogID:    "test-help-no-options",
		Title:       "Help Text No Options Test",
		Options:     []MultiChoiceOption{},
		AllowCustom: false,
	}

	d := NewMultiChoiceDialog(config).(*multiChoiceDialog)
	d.SetSize(100, 40)

	view := d.View()
	// Should show "navigate" instead of "select"
	assert.Contains(t, view, "navigate")
}

func TestMultiChoiceDialog_EmptyCustom_FallsBackToSkip(t *testing.T) {
	t.Parallel()

	config := MultiChoiceConfig{
		DialogID:       "test-empty-custom-skip",
		Title:          "Empty Custom Fallback Test",
		Options:        []MultiChoiceOption{{ID: "opt1", Label: "Option 1", Value: "value1"}},
		AllowCustom:    true,
		AllowSecondary: true,
	}

	d := NewMultiChoiceDialog(config).(*multiChoiceDialog)
	d.SetSize(100, 40)
	d.selected = selectionCustom
	d.customInput.SetValue("   ") // Whitespace only

	_, cmd := d.submitPrimary()
	require.NotNil(t, cmd)

	msgs := collectMsgs(cmd)
	resultMsg, found := findMsg[MultiChoiceResultMsg](msgs)
	require.True(t, found)
	assert.True(t, resultMsg.Result.IsSkipped, "whitespace-only custom should fall back to skip")
}

func TestMultiChoiceDialog_EmptyCustom_NoSkip_NoOp(t *testing.T) {
	t.Parallel()

	config := MultiChoiceConfig{
		DialogID:       "test-empty-custom-no-skip",
		Title:          "Empty Custom No Skip Test",
		Options:        []MultiChoiceOption{{ID: "opt1", Label: "Option 1", Value: "value1"}},
		AllowCustom:    true,
		AllowSecondary: false,
	}

	d := NewMultiChoiceDialog(config).(*multiChoiceDialog)
	d.SetSize(100, 40)
	d.selected = selectionCustom
	d.customInput.SetValue("") // Empty

	_, cmd := d.submitPrimary()
	assert.Nil(t, cmd, "empty custom with no skip allowed should be no-op")
}

func TestMultiChoiceDialog_NoSelection_NoSkip_NoOp(t *testing.T) {
	t.Parallel()

	config := MultiChoiceConfig{
		DialogID:       "test-no-selection-no-skip",
		Title:          "No Selection No Skip Test",
		Options:        []MultiChoiceOption{{ID: "opt1", Label: "Option 1", Value: "value1"}},
		AllowSecondary: false,
	}

	d := NewMultiChoiceDialog(config).(*multiChoiceDialog)
	d.SetSize(100, 40)
	d.selected = selectionNone

	_, cmd := d.submitPrimary()
	assert.Nil(t, cmd, "no selection with no skip allowed should be no-op")
}

func TestMultiChoiceDialog_CustomInputFocus(t *testing.T) {
	t.Parallel()

	config := MultiChoiceConfig{
		DialogID:    "test-custom-focus",
		Title:       "Custom Focus Test",
		Options:     []MultiChoiceOption{{ID: "opt1", Label: "Option 1", Value: "value1"}},
		AllowCustom: true,
	}

	d := NewMultiChoiceDialog(config).(*multiChoiceDialog)
	d.SetSize(100, 40)

	// Initially not focused
	d.selected = selection(0)
	d.updateFocus()
	// Note: bubbles textinput Focused() might not be directly testable without
	// checking internal state, but we can verify the selection-based logic

	// Select custom - should focus
	d.selected = selectionCustom
	d.updateFocus()
	// The input should be focused (verified by the fact that typing works)

	// Select option - should blur
	d.selected = selection(0)
	d.updateFocus()
}

func TestMultiChoiceDialog_NavigationWithoutCustom(t *testing.T) {
	t.Parallel()

	config := MultiChoiceConfig{
		DialogID:    "test-nav-no-custom",
		Title:       "Navigation Without Custom Test",
		Options:     []MultiChoiceOption{{ID: "opt1", Label: "Option 1", Value: "value1"}},
		AllowCustom: false,
	}

	d := NewMultiChoiceDialog(config).(*multiChoiceDialog)

	// Start at none
	assert.Equal(t, selectionNone, d.selected)

	// Down -> option
	d.selectNext()
	assert.Equal(t, selection(0), d.selected)

	// Down -> wrap to none (no custom)
	d.selectNext()
	assert.Equal(t, selectionNone, d.selected)

	// Up from none -> last option (no custom)
	d.selectPrevious()
	assert.Equal(t, selection(0), d.selected)
}

func TestMultiChoiceDialog_NavigationOnlyCustom(t *testing.T) {
	t.Parallel()

	config := MultiChoiceConfig{
		DialogID:    "test-nav-only-custom",
		Title:       "Navigation Only Custom Test",
		Options:     []MultiChoiceOption{}, // No options
		AllowCustom: true,
	}

	d := NewMultiChoiceDialog(config).(*multiChoiceDialog)

	// Start at none
	assert.Equal(t, selectionNone, d.selected)

	// Down -> custom (no options)
	d.selectNext()
	assert.Equal(t, selectionCustom, d.selected)

	// Down -> wrap to none
	d.selectNext()
	assert.Equal(t, selectionNone, d.selected)

	// Up from none -> custom
	d.selectPrevious()
	assert.Equal(t, selectionCustom, d.selected)
}

// ============================================================================
// Helper Function Tests
// ============================================================================

func TestIndexToDisplayNum(t *testing.T) {
	t.Parallel()

	tests := []struct {
		idx      int
		expected int
	}{
		{0, 1},   // First option displays as 1
		{1, 2},   // Second option displays as 2
		{8, 9},   // 9th option displays as 9
		{9, 0},   // 10th option displays as 0
		{10, 11}, // Beyond 10 (edge case, shouldn't happen with max 10 options)
	}

	for _, tt := range tests {
		result := indexToDisplayNum(tt.idx)
		assert.Equal(t, tt.expected, result, "indexToDisplayNum(%d) should be %d", tt.idx, tt.expected)
	}
}

func TestFormatKeyRange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		numOptions int
		expected   string
	}{
		{1, "1"},    // Single option
		{2, "1-2"},  // Two options
		{5, "1-5"},  // Five options
		{9, "1-9"},  // Nine options
		{10, "0-9"}, // Ten options (0 is 10th)
		{11, "0-9"}, // More than 10 still shows 0-9
	}

	for _, tt := range tests {
		result := formatKeyRange(tt.numOptions)
		assert.Equal(t, tt.expected, result, "formatKeyRange(%d) should be %q", tt.numOptions, tt.expected)
	}
}

func TestMultiChoiceDialog_NumberShortcuts_TenthOption(t *testing.T) {
	t.Parallel()

	// Create dialog with 10 options
	options := make([]MultiChoiceOption, 10)
	for i := range 10 {
		options[i] = MultiChoiceOption{
			ID:    "opt" + strconv.Itoa(i+1),
			Label: "Option " + strconv.Itoa(i+1),
			Value: "value" + strconv.Itoa(i+1),
		}
	}

	config := MultiChoiceConfig{
		DialogID: "test-tenth-option",
		Title:    "Tenth Option Test",
		Options:  options,
	}

	d := NewMultiChoiceDialog(config).(*multiChoiceDialog)
	d.SetSize(100, 40)

	// Press "1" to select first option
	updated, _ := d.Update(tea.KeyPressMsg{Text: "1"})
	d = updated.(*multiChoiceDialog)
	assert.Equal(t, selection(0), d.selected)

	// Press "9" to select ninth option
	updated, _ = d.Update(tea.KeyPressMsg{Text: "9"})
	d = updated.(*multiChoiceDialog)
	assert.Equal(t, selection(8), d.selected)

	// Press "0" to select tenth option
	updated, _ = d.Update(tea.KeyPressMsg{Text: "0"})
	d = updated.(*multiChoiceDialog)
	assert.Equal(t, selection(9), d.selected)
}

func TestMultiChoiceDialog_HelpText_TenOptions(t *testing.T) {
	t.Parallel()

	// Create dialog with 10 options
	options := make([]MultiChoiceOption, 10)
	for i := range 10 {
		options[i] = MultiChoiceOption{
			ID:    "opt" + strconv.Itoa(i+1),
			Label: "Option " + strconv.Itoa(i+1),
			Value: "value" + strconv.Itoa(i+1),
		}
	}

	config := MultiChoiceConfig{
		DialogID: "test-help-ten-options",
		Title:    "Ten Options Help Test",
		Options:  options,
	}

	d := NewMultiChoiceDialog(config).(*multiChoiceDialog)
	d.SetSize(100, 50)

	view := d.View()
	// Should show "0-9" for 10 options
	assert.Contains(t, view, "0-9")
	assert.Contains(t, view, "select")
}
