package tui

import (
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/markdowntransform"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

// waitMarkdownTransform reports completed display transforms, batched so a
// restored transcript refreshes at most once per interval. The channel is
// taken before views refresh, so no completion is missed.
func (m *appModel) waitMarkdownTransform() tea.Cmd {
	changed := m.markdown.Changed()
	if changed == nil {
		return nil
	}
	return func() tea.Msg {
		select {
		case <-changed:
		case <-m.markdownDone:
			return nil
		}
		batch := time.NewTimer(markdowntransform.RefreshInterval)
		defer batch.Stop()
		select {
		case <-batch.C:
			return messages.MarkdownTransformedMsg{}
		case <-m.markdownDone:
			return nil
		}
	}
}

func (m *appModel) handleMarkdownTransformed() (tea.Model, tea.Cmd) {
	wait := m.waitMarkdownTransform()
	m.viewCacheValid = false
	return m, tea.Batch(wait, m.updateAllChatsCmd(messages.MarkdownTransformedMsg{}))
}
