package dialog

import (
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// ExitConfirmedMsg is sent when the user confirms they want to exit.
type ExitConfirmedMsg struct{}

type exitConfirmationKeyMap struct {
	Yes key.Binding
	No  key.Binding
	Esc key.Binding
}

func defaultExitConfirmationKeyMap() exitConfirmationKeyMap {
	return exitConfirmationKeyMap{
		Yes: key.NewBinding(
			key.WithKeys("y", "Y", "ctrl+c"),
			key.WithHelp("Y", "yes"),
		),
		No: key.NewBinding(
			key.WithKeys("n", "N"),
			key.WithHelp("N", "no"),
		),
		Esc: key.NewBinding(
			key.WithKeys("esc"),
			key.WithHelp("Esc", "cancel"),
		),
	}
}

type exitConfirmationDialog struct {
	BaseDialog

	keyMap exitConfirmationKeyMap
}

// NewExitConfirmationDialog creates a new exit confirmation dialog.
func NewExitConfirmationDialog() Dialog {
	return &exitConfirmationDialog{
		keyMap: defaultExitConfirmationKeyMap(),
	}
}

// Init initializes the exit confirmation dialog.
func (d *exitConfirmationDialog) Init() tea.Cmd {
	return nil
}

// Update handles messages for the exit confirmation dialog.
func (d *exitConfirmationDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd

	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, d.keyMap.Yes):
			return d, tea.Sequence(
				core.CmdHandler(CloseDialogMsg{}),
				core.CmdHandler(ExitConfirmedMsg{}),
			)
		case key.Matches(msg, d.keyMap.No), key.Matches(msg, d.keyMap.Esc):
			return d, core.CmdHandler(CloseDialogMsg{})
		}
	}

	return d, nil
}

// Position returns the dialog position (centered).
func (d *exitConfirmationDialog) Position() (row, col int) {
	return d.CenterDialog(d.View())
}

// View renders the exit confirmation dialog.
func (d *exitConfirmationDialog) View() string {
	dialogWidth := d.ComputeDialogWidth(50, 30, 50)
	contentWidth := d.ContentWidth(dialogWidth, 2)

	content := NewContent(contentWidth).
		AddTitle("Exit").
		AddSeparator().
		AddSpace().
		AddQuestion("Do you want to exit?").
		AddSpace().
		AddHelpKeys("Y", "yes", "N", "no").
		Build()

	return styles.DialogStyle.
		Padding(1, 2).
		Width(dialogWidth).
		Render(content)
}
