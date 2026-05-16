package dialog

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// Layout constants for max iterations dialog.
const (
	maxIterDialogWidthPercent = 60 // Dialog width as percentage of screen
	maxIterDialogMinWidth     = 36 // Minimum dialog width
	maxIterDialogMaxWidth     = 84 // Maximum dialog width
)

type maxIterationsDialog struct {
	BaseDialog

	maxIterations int
	app           *app.App
	keyMap        ConfirmKeyMap
}

// NewMaxIterationsDialog creates a new max iterations confirmation dialog
func NewMaxIterationsDialog(maxIterations int, appInstance *app.App) Dialog {
	return &maxIterationsDialog{
		maxIterations: maxIterations,
		app:           appInstance,
		keyMap:        DefaultConfirmKeyMap(),
	}
}

// Init initializes the max iterations confirmation dialog
func (d *maxIterationsDialog) Init() tea.Cmd {
	return nil
}

// Update handles messages for the max iterations confirmation dialog
func (d *maxIterationsDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
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
				return d, tea.Sequence(
					core.CmdHandler(CloseDialogMsg{}),
					core.CmdHandler(RuntimeResumeMsg{Request: runtime.ResumeApprove()}),
				)
			},
			func() (layout.Model, tea.Cmd) {
				return d, tea.Sequence(
					core.CmdHandler(CloseDialogMsg{}),
					core.CmdHandler(RuntimeResumeMsg{Request: runtime.ResumeReject("")}),
				)
			},
		)
		if handled {
			return model, cmd
		}
	}

	return d, nil
}

// Position returns the dialog position (centered)
func (d *maxIterationsDialog) Position() (row, col int) {
	return d.CenterDialog(d.View())
}

// View renders the max iterations confirmation dialog
func (d *maxIterationsDialog) View() string {
	dialogWidth := d.ComputeDialogWidth(maxIterDialogWidthPercent, maxIterDialogMinWidth, maxIterDialogMaxWidth)
	contentWidth := dialogWidth - styles.DialogWarningStyle.GetHorizontalFrameSize()

	infoText := fmt.Sprintf("Max Iterations: %d", d.maxIterations)
	messageText := "The agent may be stuck in a loop. This can happen with smaller or less capable models."
	questionText := "Do you want to continue for 10 more iterations?"

	content := NewContent(contentWidth).
		AddTitle("Maximum Iterations Reached").
		AddSeparator().
		AddContent(styles.DialogContentStyle.Render(wrapDisplayText(infoText, contentWidth))).
		AddSpace().
		AddContent(styles.DialogContentStyle.Render(wrapDisplayText(messageText, contentWidth))).
		AddSpace().
		AddContent(styles.DialogQuestionStyle.Width(contentWidth).Render(wrapDisplayText(questionText, contentWidth))).
		AddSpace().
		AddHelpKeys("Y", "yes", "N", "no").
		Build()

	// DialogWarningStyle already includes Padding(1, 2)
	return styles.DialogWarningStyle.
		Width(dialogWidth).
		Render(content)
}

// wrapDisplayText wraps text based on display cell width.
func wrapDisplayText(text string, maxWidth int) string {
	if maxWidth <= 0 {
		return text
	}
	words := strings.Fields(text)
	if len(words) == 0 {
		return text
	}
	var lines []string
	var current string
	for _, w := range words {
		if lipgloss.Width(current) == 0 {
			current = w
			continue
		}
		if lipgloss.Width(current+" "+w) <= maxWidth {
			current += " " + w
		} else {
			lines = append(lines, current)
			current = w
		}
	}
	if current != "" {
		lines = append(lines, current)
	}
	return strings.Join(lines, "\n")
}
