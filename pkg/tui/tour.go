package tui

import (
	"log/slog"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	tourstate "github.com/docker/docker-agent/pkg/tour"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/dialog"
)

// handleStartTour starts (or restarts) the getting-started tour and records
// that the user has seen it, so the first-run offer is not shown again.
func (m *appModel) handleStartTour() (tea.Model, tea.Cmd) {
	if m.leanMode {
		return m, notification.InfoCmd("The tour needs the full TUI. Run without --lean to take it")
	}
	if err := tourstate.MarkDone(); err != nil {
		slog.Warn("Failed to persist tour state", "error", err)
	}
	m.tour.SetSize(m.width, m.height, m.contentHeight)
	m.tour.Start()
	return m, nil
}

// handleTourFinished reacts to the tour ending, either completed or quit.
func (m *appModel) handleTourFinished(completed bool) (tea.Model, tea.Cmd) {
	if completed {
		return m, notification.SuccessCmd("🎉 Tour complete. Go build something! Replay anytime with /getting-started")
	}
	return m, notification.InfoCmd("Tour closed. /getting-started brings it back anytime")
}

// handleTourOfferResult applies the user's answer to the first-run offer.
// "Not now" leaves the persisted state untouched so the offer can appear
// again on a later run; "never" persists the refusal.
func (m *appModel) handleTourOfferResult(choice dialog.TourOfferChoice) (tea.Model, tea.Cmd) {
	switch choice {
	case dialog.TourOfferAccepted:
		return m.handleStartTour()
	case dialog.TourOfferNever:
		if err := tourstate.MarkNever(); err != nil {
			slog.Warn("Failed to persist tour state", "error", err)
		}
		return m, notification.InfoCmd("Got it, never again. /getting-started if you change your mind")
	default:
		return m, notification.InfoCmd("Maybe later. /getting-started starts the tour anytime")
	}
}

var (
	tourEscapeKey = key.NewBinding(key.WithKeys("esc"))
	tourEnterKey  = key.NewBinding(key.WithKeys("enter"))
)

// handleTourKey routes the tour's two control keys while it is running: Esc
// quits the tour and Enter (on an empty editor) advances past the current
// step. Enter with draft text keeps its normal send behavior, and both keys
// are only consulted after dialogs and completions had their chance, so the
// tour never shadows a more specific interaction.
func (m *appModel) handleTourKey(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	if !m.tour.Active() {
		return nil, false
	}
	switch {
	case key.Matches(msg, tourEscapeKey):
		return m.tour.Quit(), true
	case key.Matches(msg, tourEnterKey):
		if m.focusedPanel == PanelEditor && strings.TrimSpace(m.activeTab.editor.Value()) == "" {
			return m.tour.Advance(), true
		}
	}
	return nil, false
}
