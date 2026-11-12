package dialog

import (
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// TourOfferChoice is the user's answer to the first-run tour offer.
type TourOfferChoice int

const (
	// TourOfferAccepted starts the tour now.
	TourOfferAccepted TourOfferChoice = iota
	// TourOfferLater declines for this run; the offer may be shown again.
	TourOfferLater
	// TourOfferNever declines permanently.
	TourOfferNever
)

// TourOfferResultMsg reports the user's choice on the tour offer dialog.
type TourOfferResultMsg struct {
	Choice TourOfferChoice
}

type tourOfferKeyMap struct {
	Yes   key.Binding
	Later key.Binding
	Never key.Binding
}

type tourOfferDialog struct {
	BaseDialog

	keyMap tourOfferKeyMap
	// showTelemetryNotice folds the first-run telemetry banner into the
	// offer so the two never stack.
	showTelemetryNotice bool
}

// NewTourOfferDialog creates the first-run dialog offering the
// getting-started tour.
func NewTourOfferDialog(showTelemetryNotice bool) Dialog {
	return &tourOfferDialog{
		keyMap: tourOfferKeyMap{
			Yes:   key.NewBinding(key.WithKeys("enter", "y", "Y")),
			Later: key.NewBinding(key.WithKeys("n", "N", "esc")),
			Never: key.NewBinding(key.WithKeys("d", "D")),
		},
		showTelemetryNotice: showTelemetryNotice,
	}
}

func (d *tourOfferDialog) Init() tea.Cmd {
	return nil
}

func (d *tourOfferDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd

	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, d.keyMap.Yes):
			return d, tourOfferClose(TourOfferAccepted)
		case key.Matches(msg, d.keyMap.Never):
			return d, tourOfferClose(TourOfferNever)
		case key.Matches(msg, d.keyMap.Later):
			return d, tourOfferClose(TourOfferLater)
		}
	}

	return d, nil
}

// tourOfferClose closes the dialog and reports the user's choice.
func tourOfferClose(choice TourOfferChoice) tea.Cmd {
	return tea.Sequence(
		core.CmdHandler(CloseDialogMsg{}),
		core.CmdHandler(TourOfferResultMsg{Choice: choice}),
	)
}

func (d *tourOfferDialog) Position() (row, col int) {
	return d.CenterDialog(d.View())
}

func (d *tourOfferDialog) View() string {
	dialogWidth := d.ComputeDialogWidth(50, 46, 60)
	contentWidth := d.ContentWidth(dialogWidth, 2)

	content := NewContent(contentWidth).
		AddTitle("Welcome to docker agent 👋").
		AddSeparator().
		AddSpace().
		AddContent(styles.BaseStyle.Width(contentWidth).Render(
			"First time here? Learn docker agent by doing: a hands-on tour, right in this chat. Takes two minutes, Esc leaves anytime.",
		))

	if d.showTelemetryNotice {
		content = content.
			AddSpace().
			AddContent(styles.MutedStyle.Width(contentWidth).Render(
				"Anonymous usage data helps improve docker agent. Opt out with TELEMETRY_ENABLED=false.",
			))
	}

	body := content.
		AddSpace().
		AddHelpKeys("↵", "take the tour", "N", "not now", "D", "never").
		Build()

	return styles.DialogStyle.
		Padding(1, 2).
		Width(dialogWidth).
		Render(body)
}

func (d *tourOfferDialog) Bindings() []key.Binding {
	return []key.Binding{}
}
