package dialog

import (
	"fmt"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const (
	snapshotsDialogWidthPercent = 60
	snapshotsDialogMinWidth     = 40
	snapshotsDialogMaxWidth     = 70
)

// snapshotsDialog lists every captured snapshot and lets the user reset the
// workspace to one of them (or to the original pre-agent state).
type snapshotsDialog struct {
	BaseDialog

	// fileCounts holds the number of files captured in each snapshot, oldest
	// first. An empty slice puts the dialog in its empty state.
	fileCounts []int
	// selected is the highlighted entry. 0 = <original>, N = snapshot N.
	selected int
}

// NewSnapshotsDialog creates a snapshots dialog. fileCounts must be in
// chronological order (oldest first).
func NewSnapshotsDialog(fileCounts []int) Dialog {
	return &snapshotsDialog{fileCounts: fileCounts}
}

func (d *snapshotsDialog) Init() tea.Cmd { return nil }

func (d *snapshotsDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd

	case tea.KeyPressMsg:
		if cmd := HandleQuit(msg); cmd != nil {
			return d, cmd
		}
		cmd := d.handleKey(msg)
		return d, cmd
	}
	return d, nil
}

func (d *snapshotsDialog) handleKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "esc", "q":
		return core.CmdHandler(CloseDialogMsg{})
	case "up", "k":
		if d.selected > 0 {
			d.selected--
		}
	case "down", "j":
		if d.selected < len(d.fileCounts) {
			d.selected++
		}
	case "home", "g":
		d.selected = 0
	case "end", "G":
		d.selected = len(d.fileCounts)
	case "r":
		if len(d.fileCounts) == 0 {
			return nil
		}
		return tea.Sequence(
			core.CmdHandler(CloseDialogMsg{}),
			core.CmdHandler(messages.ResetSnapshotMsg{Keep: d.selected}),
		)
	}
	return nil
}

func (d *snapshotsDialog) Position() (row, col int) {
	return d.CenterDialog(d.View())
}

func (d *snapshotsDialog) View() string {
	width := d.ComputeDialogWidth(snapshotsDialogWidthPercent, snapshotsDialogMinWidth, snapshotsDialogMaxWidth)
	inner := d.ContentWidth(width, 2)

	content := NewContent(inner).AddTitle("Snapshots").AddSeparator().AddSpace()

	if len(d.fileCounts) > 0 {
		count := pluralize(len(d.fileCounts), "snapshot", "snapshots") + " captured"
		content = content.
			AddContent(styles.DialogOptionsStyle.Width(inner).Render(count)).
			AddSpace()
	}

	body := content.
		AddContent(d.bodyContent(inner)).
		AddSpace().
		AddHelpKeys(d.helpKeys()...).
		Build()

	return styles.DialogStyle.Width(width).Render(body)
}

// bodyContent returns either the empty-state line or the snapshot list,
// depending on whether any snapshots were captured.
func (d *snapshotsDialog) bodyContent(inner int) string {
	if len(d.fileCounts) == 0 {
		return styles.DialogContentStyle.
			Italic(true).
			Foreground(styles.TextMuted).
			Width(inner).
			Align(lipgloss.Center).
			Render("No snapshots taken yet.")
	}

	rows := make([]string, 0, len(d.fileCounts)+1)
	rows = append(rows, d.renderRow("<original>", "restore the initial state", d.selected == 0, inner))
	for i, count := range d.fileCounts {
		rows = append(rows, d.renderRow(
			fmt.Sprintf("Snapshot %d", i+1),
			pluralize(count, "file", "files"),
			d.selected == i+1,
			inner,
		))
	}
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

func (d *snapshotsDialog) helpKeys() []string {
	if len(d.fileCounts) == 0 {
		return []string{"esc", "close"}
	}
	return []string{"↑/↓", "navigate", "r", "restore", "esc", "close"}
}

// renderRow draws a single list entry with the name on the left and a short
// description right-aligned within width.
func (d *snapshotsDialog) renderRow(name, desc string, selected bool, width int) string {
	nameStyle, descStyle := styles.PaletteUnselectedActionStyle, styles.PaletteUnselectedDescStyle
	if selected {
		nameStyle, descStyle = styles.PaletteSelectedActionStyle, styles.PaletteSelectedDescStyle
	}
	left := nameStyle.Render(" " + name + " ")
	right := descStyle.Render(" " + desc + " ")
	gap := max(0, width-lipgloss.Width(left))
	return left + lipgloss.PlaceHorizontal(gap, lipgloss.Right, right,
		lipgloss.WithWhitespaceStyle(descStyle))
}

func pluralize(n int, singular, plural string) string {
	word := plural
	if n == 1 {
		word = singular
	}
	return fmt.Sprintf("%d %s", n, word)
}
