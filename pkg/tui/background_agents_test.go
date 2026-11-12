package tui

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
)

func TestBackgroundAgentStatusStaysOnOwningTab(t *testing.T) {
	t.Parallel()

	m, activeID, hiddenID := newRoutedTestModel(t, func(sess *session.Session, routingID string) chat.Page {
		return newRealChatPage(t, sess, routingID)
	})
	for _, id := range []string{activeID, hiddenID} {
		m.tabs[id].sessionState.SetCurrentAgentName("root")
		_, _ = m.Update(messages.RoutedMsg{SessionID: id, Inner: runtime.BackgroundAgentStatus("child-"+id, "worker", true)})
		assert.Equal(t, "root", m.tabs[id].sessionState.CurrentAgentName())
		assert.Contains(t, ansi.Strip(m.tabs[id].chatPage.View()), "Background: 1 running")
		assert.False(t, m.tabs[id].chatPage.IsWorking())
		_, _ = m.Update(messages.RoutedMsg{SessionID: id, Inner: runtime.NewTokenUsageEvent("child-"+id, "worker", &runtime.Usage{
			ContextLength: 10, ContextLimit: 100,
		})})
		assert.Equal(t, "root", m.tabs[id].sessionState.CurrentAgentName(), "background usage must not select the worker")
	}
	_, _ = m.Update(messages.RoutedMsg{SessionID: hiddenID, Inner: runtime.BackgroundAgentStatus("child-"+hiddenID, "worker", false)})
	assert.NotContains(t, m.tabs[hiddenID].chatPage.View(), "Background:")
	assert.Contains(t, ansi.Strip(m.tabs[activeID].chatPage.View()), "Background: 1 running")
	_, _ = m.Update(runtime.BackgroundAgentStatus("child-"+activeID, "worker", false))
	assert.NotContains(t, m.tabs[activeID].chatPage.View(), "Background:")
	assert.Equal(t, "root", m.activeTab.sessionState.CurrentAgentName())
}

func TestBackgroundUsagePreservesSelectedAgent(t *testing.T) {
	t.Parallel()

	for _, routed := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "routed"}[routed], func(t *testing.T) {
			t.Parallel()
			m, activeID, _ := newRoutedTestModel(t, func(sess *session.Session, routingID string) chat.Page {
				return newRealChatPage(t, sess, routingID)
			})
			m.activeTab.sessionState.SetCurrentAgentName("root")
			for _, event := range []runtime.Event{
				runtime.NewTokenUsageEvent("child", "worker", &runtime.Usage{ContextLength: 10, ContextLimit: 100}),
				&runtime.EvaluationUsageEvent{AgentContext: runtime.AgentContext{AgentName: "worker"}, SessionID: "child"},
			} {
				if routed {
					_, _ = m.Update(messages.RoutedMsg{SessionID: activeID, Inner: event})
				} else {
					_, _ = m.Update(event)
				}
				assert.Equal(t, "root", m.activeTab.sessionState.CurrentAgentName())
				assert.False(t, m.activeTab.chatPage.IsWorking())
			}
		})
	}
}
