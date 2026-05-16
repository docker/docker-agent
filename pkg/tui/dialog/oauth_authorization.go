package dialog

import (
	"context"
	"fmt"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type oauthAuthorizationDialog struct {
	BaseDialog

	serverURL string
	app       *app.App
	keyMap    ConfirmKeyMap
}

// NewOAuthAuthorizationDialog creates a new OAuth authorization confirmation dialog
func NewOAuthAuthorizationDialog(serverURL string, appInstance *app.App) Dialog {
	return &oauthAuthorizationDialog{
		serverURL: serverURL,
		app:       appInstance,
		keyMap:    DefaultConfirmKeyMap(),
	}
}

// Init initializes the OAuth authorization confirmation dialog
func (d *oauthAuthorizationDialog) Init() tea.Cmd {
	return nil
}

// Update handles messages for the OAuth authorization confirmation dialog
func (d *oauthAuthorizationDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd

	case tea.KeyPressMsg:
		if cmd := HandleQuit(msg); cmd != nil {
			return d, cmd
		}

		model, cmd, handled := HandleConfirmKeys(msg, d.keyMap,
			func() (layout.Model, tea.Cmd) {
				_ = d.app.ResumeElicitation(context.Background(), tools.ElicitationActionAccept, nil)
				return d, core.CmdHandler(CloseDialogMsg{})
			},
			func() (layout.Model, tea.Cmd) {
				_ = d.app.ResumeElicitation(context.Background(), tools.ElicitationActionDecline, nil)
				return d, core.CmdHandler(CloseDialogMsg{})
			},
		)
		if handled {
			return model, cmd
		}
	}

	return d, nil
}

// Position returns the dialog position (centered)
func (d *oauthAuthorizationDialog) Position() (row, col int) {
	return d.CenterDialog(d.View())
}

// View renders the OAuth authorization confirmation dialog
func (d *oauthAuthorizationDialog) View() string {
	dialogWidth := d.ComputeDialogWidth(60, 40, 90)
	contentWidth := d.ContentWidth(dialogWidth, 2)

	serverInfo := styles.InfoStyle.
		Width(contentWidth).
		Render(fmt.Sprintf("Server: %s (remote)", d.serverURL))

	description := styles.DialogContentStyle.
		Width(contentWidth).
		Render("This server requires OAuth authentication to access its tools. Your browser will open automatically to complete the authorization process.")

	instructions := "After authorizing in your browser, return here and the agent will continue automatically."

	content := NewContent(contentWidth).
		AddContent(styles.DialogTitleInfoStyle.Width(contentWidth).Render("\U0001F510 OAuth Authorization Required")).
		AddSpace().
		AddContent(serverInfo).
		AddSpace().
		AddContent(description).
		AddSpace().
		AddHelp(instructions).
		AddSpace().
		AddHelpKeys("Y", "authorize", "N", "decline").
		Build()

	return styles.DialogWarningStyle.
		Padding(1, 2).
		Width(dialogWidth).
		Render(content)
}
