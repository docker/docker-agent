package tui

import (
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"

	"github.com/docker/docker-agent/pkg/programstatus"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
)

func (m *appModel) refreshProgramStatus() tea.Cmd {
	if m.programStatus == nil || m.programStatusClosing || m.programStatusProbe || m.programStatusUnsupported {
		return nil
	}
	statuses := make([]programstatus.Status, 0, len(m.tabs))
	for _, id := range m.programStatusTabIDs() {
		if tab := m.tabs[id]; tab != nil {
			statuses = append(statuses, tab.programStatus.Status())
		}
	}
	if !m.programStatus.Set(programstatus.Aggregate(statuses)) {
		return nil
	}
	return tea.Raw(m.programStatus)
}

func (m *appModel) acknowledgeProgramStatus(msg tea.Msg) {
	if m.programStatus == nil || m.activeTab == nil {
		return
	}
	switch msg.(type) {
	case tea.KeyPressMsg, tea.PasteMsg, tea.MouseClickMsg:
		m.activeTab.programStatus.Acknowledge()
	}
}

func (m *appModel) retireProgramStatus() tea.Cmd {
	if m.programStatus == nil || m.programStatusProbe || m.programStatusUnsupported {
		return nil
	}
	m.programStatusClosing = true
	statuses := make([]programstatus.Status, 0, len(m.tabs))
	for _, id := range m.programStatusTabIDs() {
		if tab := m.tabs[id]; tab != nil {
			status := tab.programStatus.Status()
			if status.State == "done" || status.State == "error" {
				statuses = append(statuses, status)
			}
		}
	}
	m.programStatus.Set(programstatus.Aggregate(statuses))
	return tea.Raw(m.programStatus)
}

func (m *appModel) programStatusTabIDs() []string {
	ids := make([]string, 0, len(m.tabs))
	for id := range m.tabs {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

type programStatusTimeoutMsg struct{}

func (m *appModel) programStatusProbeCmd() tea.Cmd {
	if !m.programStatusProbe {
		return nil
	}
	// Other startup queries also use DA1, so only our OSC reply proves support.
	return tea.Sequence(tea.Raw("\x1b]7501;?\x1b\\"), tea.Tick(200*time.Millisecond, func(time.Time) tea.Msg {
		return programStatusTimeoutMsg{}
	}))
}

func (m *appModel) handleProgramStatusReply(msg tea.Msg) bool {
	switch ev := msg.(type) {
	case programStatusTimeoutMsg:
		if m.programStatusProbe {
			m.programStatusProbe = false
			m.programStatusUnsupported = true
		}
		return true
	case uv.UnknownOscEvent:
		if !m.programStatusProbe {
			return false
		}
		body := strings.TrimSuffix(strings.TrimSuffix(string(ev), "\x1b\\"), "\a")
		if strings.HasPrefix(body, "\x1b]7501;?") {
			m.programStatusProbe = false
			return true
		}
	}
	return false
}

func (m *appModel) runLifecycleOption(tab *tabModel) chat.PageOption {
	return chat.WithRunLifecycle(func() {
		if m.programStatus != nil {
			tab.programStatus.Start(tab.state.SessionID())
		}
	}, func() {
		if m.programStatus != nil {
			tab.programStatus.Cancel(tab.state.SessionID())
		}
	}, func(reason string) {
		if m.programStatus != nil {
			tab.programStatus.FinishSetup(tab.state.SessionID(), reason)
		}
	})
}
