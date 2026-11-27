package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/components/contextbar"
)

func (m *appModel) ensureContextBar() {
	if m.contextBar == nil && m.ar != nil {
		m.contextBar = contextbar.New(m.ar)
		if used, limit, known := m.activeTab.chatPage.ActiveContextUsage(); known && !m.leanMode {
			m.contextBar.SetContextUsageDirect(used, limit)
		}
	}
}

func (m *appModel) syncComposerContext() tea.Cmd {
	if m.contextBar == nil {
		return nil
	}
	if m.viewCacheValid {
		label := m.contextBar.Label()
		defer func() {
			if label != m.contextBar.Label() {
				m.viewCacheValid = false
			}
		}()
	}
	if m.leanMode {
		m.contextBar.SetContextUsageDirect(0, 0)
		return nil
	}
	if m.startupContextPage != nil && m.startupContextPage != m.activeTab.chatPage {
		m.startupContextPage = nil
	}
	used, limit, known := m.activeTab.chatPage.ActiveContextUsage()
	if !known && m.activeConversationStarted() {
		// Until a conversation reports its own usage, keep the last visible
		// fill as the origin instead of restarting from an empty track.
		m.contextBar.Hold()
		return nil
	}
	// A restored startup conversation has no previously displayed fill to sweep from.
	direct := m.startupContextPage != nil || m.paneHidden || m.composer.stripRows == 0
	if !known || limit <= 0 {
		// Empty conversations have no usage to wait for, and usage without a
		// context window has no fill: ease to an empty track.
		if !direct {
			return m.contextBar.SetContextUsage(0, 0)
		}
		// Clearing a held fill must repaint even when the event would
		// otherwise reuse the cached view.
		if m.contextBar.Filled() {
			m.viewCacheValid = false
		}
		m.contextBar.SetContextUsageDirect(0, 0)
		return nil
	}
	if direct {
		m.startupContextPage = nil
		m.contextBar.SetContextUsageDirect(used, limit)
		return nil
	}
	return m.contextBar.SetContextUsage(used, limit)
}

func (m *appModel) activeConversationStarted() bool {
	if m.application == nil || m.application.Session() == nil {
		return false
	}
	sess := m.application.Session()
	input, output := sess.Usage()
	return input != 0 || output != 0 || sess.MessageCount() > 0
}
