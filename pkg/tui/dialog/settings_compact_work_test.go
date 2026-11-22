package dialog

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/messages"
)

// The Compact work log row is a Behavior toggle that a mouse click flips and a
// second click restores, using the row geometry the dialog draws.
func TestSettingsDialogCompactWorkRowMouseToggles(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	d.tab = tabBehavior
	d.View()
	before := d.current.CompactWorkLog

	d.Update(settingsRowClick(t, d, "Compact work log"))
	assert.NotEqual(t, before, d.current.CompactWorkLog, "a click on the row toggles it")

	d.Update(settingsRowClick(t, d, "Compact work log"))
	assert.Equal(t, before, d.current.CompactWorkLog, "a second click restores it")
}

// Visible replies is a Behavior selector: a mouse click on the row cycles the
// policy and reports it live as a preview; the default is "Last 3".
func TestSettingsDialogVisibleRepliesRowMouseCycles(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	d.tab = tabBehavior
	d.View()
	assert.Equal(t, messages.VisibleRepliesThree, messages.ParseVisibleReplies(string(d.current.VisibleReplies)))

	_, cmd := d.Update(settingsRowClick(t, d, "Visible replies"))
	msgs := collectMsgs(cmd)
	require.Len(t, msgs, 1)
	preview, ok := msgs[0].(messages.PreviewVisibleRepliesMsg)
	require.True(t, ok)
	assert.Equal(t, messages.VisibleRepliesFive, preview.Policy, "the first click moves from Last 3 to Last 5")
	assert.Equal(t, messages.VisibleRepliesFive, d.current.VisibleReplies)
}

// Escape after a visible-replies preview must hand back the policy that was
// open before the dialog, so the transcript returns to it.
func TestSettingsDialogVisibleRepliesCancelRestoresOriginal(t *testing.T) {
	t.Parallel()

	d := newTestSettingsDialog(t, messages.LayoutSettings{})
	// The real open path always carries the saved policy; the fixture must too.
	d.original.VisibleReplies = messages.VisibleRepliesThree
	d.current.VisibleReplies = messages.VisibleRepliesThree
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	d.tab = tabBehavior
	d.View()

	_, _ = d.Update(settingsRowClick(t, d, "Visible replies"))
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	var cancel *messages.CancelLayoutPreviewMsg
	for _, msg := range collectMsgs(cmd) {
		if c, ok := msg.(messages.CancelLayoutPreviewMsg); ok {
			cancel = &c
		}
	}
	require.NotNil(t, cancel, "escape after a change emits the cancel preview")
	assert.Equal(t, messages.VisibleRepliesThree, cancel.OriginalVisibleReplies)
}

func settingsRowClick(t *testing.T, d *settingsDialog, label string) tea.MouseClickMsg {
	t.Helper()
	lines := strings.Split(ansi.Strip(d.View()), "\n")
	row, col := d.Position()
	for y, line := range lines {
		if x := strings.Index(line, label); x >= 0 {
			return tea.MouseClickMsg{X: col + lipgloss.Width(line[:x]), Y: row + y, Button: tea.MouseLeft}
		}
	}
	require.Failf(t, "row not rendered", "%q is not visible", label)
	return tea.MouseClickMsg{}
}
