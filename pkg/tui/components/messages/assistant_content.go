package messages

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/message"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func (m *model) trackMessageIdentity(sessionID, messageID string) {
	for _, last := range slices.Backward(m.messages) {
		if last.Type == types.MessageTypeSpinner {
			continue
		}
		if last.SessionID != sessionID || last.MessageID != messageID {
			m.BreakMessageGroup()
		}
		return
	}
}

// AppendAssistantContent keeps separate attempts and interleaved sessions distinct.
func (m *model) AppendAssistantContent(sessionID, messageID, agentName, content string) tea.Cmd {
	m.trackMessageIdentity(sessionID, messageID)
	cmd := m.AppendToLastMessage(agentName, content)
	if last := m.lastMessage(); last != nil {
		last.SessionID, last.MessageID = sessionID, messageID
	}
	return cmd
}

// AppendReasoningContent uses the same logical boundary as assistant content.
func (m *model) AppendReasoningContent(sessionID, messageID, agentName, content string) tea.Cmd {
	m.trackMessageIdentity(sessionID, messageID)
	cmd := m.AppendReasoning(agentName, content)
	if last := m.lastMessage(); last != nil {
		last.SessionID, last.MessageID = sessionID, messageID
	}
	return cmd
}

// ResetReasoningContent clears every matching segment, including copyable backing text.
func (m *model) ResetReasoningContent(sessionID, messageID, agentName string) tea.Cmd {
	materialize := m.materializeDeferredTail()
	for i, msg := range slices.Backward(m.messages) {
		if msg.Type != types.MessageTypeAssistantReasoningBlock || msg.SessionID != sessionID || msg.MessageID != messageID || msg.Sender != agentName {
			continue
		}
		block := m.views[i].(*reasoningblock.Model)
		block.ClearReasoning()
		msg.Content = ""
		if block.ToolCount() == 0 {
			animation.StopView(block)
			m.messages = slices.Delete(m.messages, i, i+1)
			m.views = slices.Delete(m.views, i, i+1)
			if i < m.groupStart {
				m.groupStart--
			}
		}
	}
	m.invalidateAllItems()
	return materialize
}

// ReconcileAssistantContent replaces incomplete streamed text with the saved answer.
func (m *model) ReconcileAssistantContent(sessionID, messageID, agentName, content string) tea.Cmd {
	if content == "" {
		return nil
	}
	materialize := m.materializeDeferredTail()
	var indices []int
	var streamed strings.Builder
	for i, msg := range m.messages {
		if msg.Type == types.MessageTypeAssistant && msg.SessionID == sessionID && msg.MessageID == messageID && msg.Sender == agentName {
			indices = append(indices, i)
			streamed.WriteString(msg.Content)
		}
	}
	content = strings.ReplaceAll(content, "\t", "    ")
	if len(indices) == 0 {
		return tea.Batch(materialize, m.AppendAssistantContent(sessionID, messageID, agentName, content))
	}
	if streamed.String() == content {
		return materialize
	}
	// Keep segment boundaries when the canonical text extends the delivered prefix.
	var cmds []tea.Cmd
	cmds = append(cmds, materialize)
	if strings.HasPrefix(content, streamed.String()) {
		i := indices[len(indices)-1]
		msg := *m.messages[i]
		msg.Content += strings.TrimPrefix(content, streamed.String())
		m.messages[i] = &msg
		cmds = append(cmds, m.views[i].(message.Model).SetMessage(&msg))
		m.invalidateItem(i)
	} else {
		for n, i := range indices {
			msg := *m.messages[i]
			msg.Content = ""
			if n == 0 {
				msg.Content = content
			}
			m.messages[i] = &msg
			cmds = append(cmds, m.views[i].(message.Model).SetMessage(&msg))
			m.invalidateItem(i)
		}
	}
	return tea.Batch(cmds...)
}

// AppendAssistantMediaContent keeps media joined to its logical text message.
func (m *model) AppendAssistantMediaContent(sessionID, messageID, agentName string, media []types.AssistantMedia) tea.Cmd {
	for i, msg := range slices.Backward(m.messages) {
		legacyMatch := msg.MessageID == "" && strings.HasPrefix(messageID, "legacy:") && slices.ContainsFunc(msg.AssistantMedia, func(existing types.AssistantMedia) bool {
			return existing.Key != "" && slices.ContainsFunc(media, func(item types.AssistantMedia) bool { return item.Key == existing.Key })
		})
		if msg.Type != types.MessageTypeAssistant || msg.SessionID != sessionID || (msg.MessageID != messageID && !legacyMatch) || msg.Sender != agentName {
			continue
		}
		var added []types.AssistantMedia
		for _, item := range media {
			if item.Key != "" && slices.ContainsFunc(msg.AssistantMedia, func(existing types.AssistantMedia) bool { return existing.Key == item.Key }) {
				continue
			}
			added = append(added, item)
		}
		if len(added) == 0 {
			return nil
		}
		updated := *msg
		updated.AssistantMedia = slices.Concat(msg.AssistantMedia, added)
		m.messages[i] = &updated
		cmd := m.views[i].(message.Model).SetMessage(&updated)
		m.invalidateItem(i)
		return cmd
	}
	m.trackMessageIdentity(sessionID, messageID)
	cmd := m.AppendAssistantMedia(agentName, media)
	if last := m.lastMessage(); last != nil {
		last.SessionID, last.MessageID = sessionID, messageID
	}
	return cmd
}

// AdoptAssistantMediaIdentity joins legacy canonical replay to restored manifest media.
func (m *model) AdoptAssistantMediaIdentity(sessionID, messageID, agentName string, media []types.AssistantMedia) {
	if !strings.HasPrefix(messageID, "legacy:") {
		return
	}
	for _, msg := range m.messages {
		if msg.Type != types.MessageTypeAssistant || msg.SessionID != sessionID || msg.MessageID != "" || msg.Sender != agentName {
			continue
		}
		if slices.ContainsFunc(msg.AssistantMedia, func(existing types.AssistantMedia) bool {
			return existing.Key != "" && slices.ContainsFunc(media, func(item types.AssistantMedia) bool { return item.Key == existing.Key })
		}) {
			msg.MessageID = messageID
			return
		}
	}
}
